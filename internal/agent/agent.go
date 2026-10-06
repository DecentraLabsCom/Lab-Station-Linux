package agent

import (
	"context"
	"crypto/sha256"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/decentralabs/lab-station-linux/internal/config"
	"github.com/decentralabs/lab-station-linux/internal/host"
)

type Agent struct {
	Config config.Config
	Runtime host.Runtime
	runHelper func(context.Context, []byte) error
	rebootMarker func() bool
	mu sync.Mutex
}

type Result struct {
	ID string `json:"id"`
	Command string `json:"command"`
	CompletedAt string `json:"completedAt"`
	Success bool `json:"success"`
	ExitCode int `json:"exitCode"`
	Outcome string `json:"outcome"`
	Message string `json:"message"`
	Options []string `json:"options,omitempty"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	DurationMs int64 `json:"durationMs"`
	Metadata map[string]any `json:"metadata"`
}

type Request struct {
	SchemaVersion int `json:"schemaVersion"`
	ID string `json:"id"`
	Operation string `json:"operation"`
	Command string `json:"command"`
	Args []string `json:"args"`
	Artifact string `json:"artifact"`
	SecretID string `json:"secretId"`
	SecretValue string `json:"secretValue"`
}

func New(cfg config.Config) *Agent { return &Agent{Config: cfg, Runtime: host.NewRuntime()} }
func (a *Agent) hostRuntime() host.Runtime { if a.Runtime != nil { return a.Runtime }; return host.NewRuntime() }

func (a *Agent) Execute(ctx context.Context, id, command string, args []string) Result {
	started := time.Now()
	if id == "" { id = newID() }
	code, stdout, stderr, metadata := a.execute(ctx, command, args)
	outcome := "success"
	if code == 1 { outcome = "warning" } else if code >= 2 { outcome = "failure" }
	message := outcome
	if strings.TrimSpace(stdout) != "" { lines := strings.Split(strings.TrimSpace(stdout), "\n"); message = lines[len(lines)-1] }
	if strings.TrimSpace(stderr) != "" && code >= 1 { message = strings.TrimSpace(stderr) }
	result := Result{ID:id, Command:command, CompletedAt:time.Now().UTC().Format(time.RFC3339Nano), Success:code < 2, ExitCode:code, Outcome:outcome, Message:message, Options:append([]string(nil), args...), Stdout:stdout, Stderr:stderr, DurationMs:time.Since(started).Milliseconds(), Metadata:metadata}
	a.recordOperation(result)
	return result
}

func (a *Agent) execute(ctx context.Context, command string, args []string) (int, string, string, map[string]any) {
	if err := ValidateCommand(command, args); err != nil { return 2, "", err.Error(), map[string]any{"code":"STATION_COMMAND_REJECTED"} }
	switch command {
	case "identity":
		status := a.Status()
		identity := map[string]any{"host": status["host"], "version": status["version"], "contractVersion":"3.0.0", "platform":status["platform"], "management":status["management"]}
		encoded, _ := json.Marshal(identity)
		return 0, string(encoded), "", map[string]any{"contractVersion":"3.0.0", "platform":"linux"}
	case "status-json":
		encoded, _ := json.Marshal(a.Status())
		return 0, string(encoded), "", map[string]any{"contractVersion":"3.0.0"}
	case "prepare-session": return a.prepare(ctx, args)
	case "release-session": return a.release(ctx, args)
	case "session guard": return a.guard(ctx, args)
	case "local-mode": return a.localMode(args[0],args[1:])
	case "power": return a.power(ctx, args)
	case "recovery reboot-if-needed": return a.recovery(ctx, args)
	case "energy audit":
		encoded, _ := json.Marshal(a.energyAudit())
		return 0, string(encoded), "", map[string]any{"backend":"sysfs+logind"}
	case "service": return a.service(ctx, args[0])
	case "fmu-executor": return a.fmu(ctx, args[0])
	default: return 2, "", "Station command is not implemented", map[string]any{"code":"STATION_COMMAND_REJECTED"}
	}
}

func ValidateCommand(command string, args []string) error {
	if len(args) > 12 { return errors.New("too many command arguments") }
	optionArgs:=args
	validOption:=map[string]bool{}
	switch command {
	case "prepare-session":
		validOption=map[string]bool{"--guard-grace":true,"--guard-message":true,"--guard-silent":true,"--guard-notify":true,"--no-guard":true,"--message":true,"--grace":true,"--silent":true,"--no-notify":true,"--reservation-key":true,"--operation-id":true}
	case "release-session":
		validOption=map[string]bool{"--reboot":true,"--reboot-timeout":true,"--reservation-key":true,"--operation-id":true}
	case "session guard":
		validOption=map[string]bool{"--guard-grace":true,"--guard-message":true,"--guard-silent":true,"--guard-notify":true,"--no-guard":true,"--user":true,"--message":true,"--grace":true,"--silent":true,"--no-notify":true,"--operation-id":true}
	case "power":
		if len(args)==0 || !oneOf(args[0],"shutdown","hibernate") { return errors.New("power action is not allowlisted") }
		optionArgs=args[1:]
		validOption=map[string]bool{"--delay":true,"--reason":true,"--require-wake":true}
	case "recovery reboot-if-needed": validOption=map[string]bool{"--force":true,"--timeout":true,"--reason":true}
	case "local-mode":
		if len(args)==1 && oneOf(args[0],"clear","status") { return nil }
		if len(args)==1&&args[0]=="set"{return nil}
		if len(args)==2&&args[0]=="set"&&strings.HasPrefix(args[1],"--ttl="){seconds,err:=strconv.Atoi(strings.TrimPrefix(args[1],"--ttl="));if err==nil&&seconds>=60&&seconds<=86400{return nil}}
		return errors.New("local-mode action is not allowlisted")
	case "service":
		if len(args)==1 && oneOf(args[0],"status","start","stop") { return nil }
		return errors.New("service action is not allowlisted")
	case "fmu-executor":
		if len(args)==1 && oneOf(args[0],"status","start","stop","restart") { return nil }
		return errors.New("FMU Executor action is not allowlisted")
	case "identity", "status-json", "energy audit":
		if len(args)==0 { return nil }
		return errors.New("this command takes no arguments")
	default: return errors.New("command is not allowlisted")
	}
	for _, arg := range optionArgs {
		if len(arg) > 256 || strings.ContainsAny(arg, "\r\n\x00") { return errors.New("invalid command argument") }
		if strings.ContainsAny(arg, "`$|;&><") { return errors.New("shell metacharacters are not allowed") }
		key := strings.SplitN(arg, "=", 2)[0]
		if !strings.HasPrefix(arg, "--") || !validOption[key] { return errors.New("command argument is not allowlisted") }
		if command == "session guard" && key == "--user" { _, value, hasValue := strings.Cut(arg, "="); if !hasValue || !validLocalUsername(value) { return errors.New("session guard user is invalid") } }
	}
	switch command {
	case "prepare-session", "release-session", "session guard": return nil
	case "power", "recovery reboot-if-needed": return nil
	}
	return errors.New("command is not allowlisted")
}

func (a *Agent) Status() map[string]any {
	distro, distroVersion := host.DetectDistro()
	platform := a.hostRuntime()
	supervisor := platform.Supervisor()
	initName := "unknown"
	if supervisor != nil { initName = supervisor.Name() }
	interfaces := platform.NetworkInterfaces()
	localSessions, remoteSessions, sessionsOK := platform.Sessions()
	xrdpState := serviceState(supervisor, "xrdp.service")
	if xrdpState == "unknown" { xrdpState = serviceState(supervisor, "xrdp") }
	applicationConfigured := a.Config.Profile != "fmu-only" && a.applicationAvailable()
	remoteReady := applicationConfigured && xrdpState == "active" && a.tinyDeskConfigurationReady()
	physicalAvailable := a.Config.Profile != "fmu-only"
	fmuState := serviceState(supervisor, "decentralabs-labstation-fmu.service")
	fmuConfigured := fileExists("/etc/decentralabs/lab-station/secrets/fmu-internal-token.env")
	fmuReady := fmuConfigured && fmuState == "active"
	wakeSupported, wakeEnabled := platform.WakeStatus(interfaces)
	wakePersistent := platform.WakePersistent(interfaces)
	wakeIssues := []string{}
	if !wakeSupported { wakeIssues = append(wakeIssues, "no network interface reports Wake-on-LAN support") }
	if wakeSupported && !wakeEnabled { wakeIssues = append(wakeIssues, "Wake-on-LAN is not enabled on a supported interface") }
	if wakeSupported && wakeEnabled && !wakePersistent { wakeIssues = append(wakeIssues, "Wake-on-LAN persistence is not managed or enabled") }
	physicalIssues := []string{}
	if physicalAvailable && !applicationConfigured { physicalIssues = append(physicalIssues, "configured application is unavailable") }
	if physicalAvailable && xrdpState != "active" { physicalIssues = append(physicalIssues, "xrdp service is not active") }
	if physicalAvailable && !a.tinyDeskConfigurationReady() { physicalIssues = append(physicalIssues, "Tiny Desk session or RDP policy is not ready; verify the labuser password and xrdp channel restrictions") }
	if physicalAvailable && !sessionsOK { physicalIssues = append(physicalIssues, "login session inventory is unavailable") }
	if physicalAvailable && len(localSessions) > 0 {
		if a.Config.Profile == "dedicated" { physicalIssues = append(physicalIssues, "local user session is active on a dedicated station") }
		if a.Config.Profile == "hybrid" && !a.Config.AllowLocalSessionEviction { physicalIssues = append(physicalIssues, "local session is active and session eviction is disabled") }
	}
	physicalReady := physicalAvailable && remoteReady && sessionsOK
	if a.Config.Profile == "dedicated" && len(localSessions) > 0 { physicalReady = false }
	if a.Config.Profile == "hybrid" && len(localSessions) > 0 && !a.Config.AllowLocalSessionEviction { physicalReady = false }
	ready := physicalReady
	if a.Config.Profile == "fmu-only" { ready = fmuReady }
	stateName := "ready"
	if !ready { stateName = "degraded" }
	if !physicalAvailable && !fmuReady { stateName = "unavailable" }
	issues := append([]string{}, physicalIssues...)
	if a.Config.Profile == "fmu-only" { issues = []string{}; if !fmuReady { issues = append(issues, "FMU Executor is not ready") } }
	remoteMode := "tiny-desk"
	if a.Config.Profile == "fmu-only" { remoteMode = "none"; physicalIssues = []string{"profile is fmu-only"} }
	active := make([]map[string]any, 0, len(localSessions)+len(remoteSessions))
	active = append(active, localSessions...)
	active = append(active, remoteSessions...)
	status := map[string]any{
		"schemaVersion":"3.0.0", "timestamp":time.Now().UTC().Format(time.RFC3339Nano), "host":a.Config.Name, "version":a.Config.Version,
		"platform":map[string]any{"os":"linux", "distro":distro, "distroVersion":distroVersion, "arch":architecture(), "kernel":platform.KernelRelease(), "init":initName, "capabilities":[]string{"ssh-dispatcher", "status-v3", "heartbeat", "local-mode", "power"}},
		"profile":a.Config.Profile,
		"management":map[string]any{"transport":"ssh", "port":a.Config.ManagementPort, "ready":serviceState(supervisor,"ssh.service")=="active" || serviceState(supervisor,"sshd.service")=="active", "dispatcher":true},
		"remoteAccess":map[string]any{"mode":remoteMode, "backend":"xrdp-xorg", "available":physicalAvailable, "ready":remoteReady, "issues":physicalIssues, "applicationId":a.Config.Application.ID, "user":a.Config.Application.User},
		"summary":map[string]any{"state":stateName, "ready":ready, "issues":issues},
		"readiness":map[string]any{
			"physicalLab":host.Capability{Available:physicalAvailable, Ready:physicalReady, Backend:"tiny-desk", Issues:physicalIssues},
			"wake":host.Capability{Available:wakeSupported, Ready:wakeSupported && wakeEnabled && wakePersistent, Backend:"ethtool", Issues:wakeIssues},
			"fmu":host.Capability{Available:fileExists("/opt/decentralabs/fmu-executor/app/main.py"), Ready:fmuReady, Backend:"native", Issues:capabilityIssues(fmuReady,"FMU Executor is not installed or its token/service is unavailable")},
			"remoteAccess":host.Capability{Available:physicalAvailable, Ready:remoteReady, Backend:"xrdp-xorg", Issues:physicalIssues},
		},
		"sessions":map[string]any{"active":active, "localSessionActive":len(localSessions)>0, "remoteSessionActive":hasLabUserRemote(remoteSessions), "labUserActive":hasLabUserRemote(remoteSessions), "labUserRemoteActive":hasLabUserRemote(remoteSessions), "localActive":len(localSessions), "remoteActive":len(remoteSessions), "queryOk":sessionsOK},
		"operations":a.latestOperation(), "localModeEnabled":a.localModeEnabled(),
		"identity":map[string]any{"hostname":a.Config.Name,"agentVersion":a.Config.Version,"contractVersion":"3.0.0"},
		"localGraphics":map[string]any{"present":platform.LocalGraphicsPresent(), "displayManager":"not_modified_by_lab_station"},
		"application":map[string]any{"id":a.Config.Application.ID,"configured":applicationConfigured,"available":applicationConfigured,"commandHash":a.applicationHash(),"configHash":a.configurationHash()},
		"policy":map[string]any{"setup":"managed","privilegedHelper":fileExists("/usr/lib/decentralabs/lab-station/labstation-helper"),"allowLocalSessionEviction":a.Config.AllowLocalSessionEviction},
		"wake":map[string]any{"interfaces":interfaces,"supported":wakeSupported,"enabled":wakeEnabled,"persistent":wakePersistent,"complianceIssues":wakeIssues},
		"power":a.powerCapabilities(),
		"fmuExecutor":map[string]any{"mode":"native","available":fileExists("/opt/decentralabs/fmu-executor/app/main.py"),"version":fileText("/opt/decentralabs/fmu-executor/VERSION"),"serviceState":fmuState,"ready":fmuReady,"tokenConfigured":fmuConfigured,"port":8091},
		"supportTier":"unverified",
		"platformSpecific":map[string]any{"linux":map[string]any{"supervisor":initName,"packageManager":firstValue(host.DetectPackageManager())}},
	}
	return status
}

func (a *Agent) Artifact(name string) ([]byte, error) {
	var path string
	switch name { case "heartbeat", "status": path=filepath.Join(a.Config.StateDir,"heartbeat.json"); case "session-events": path=filepath.Join(a.Config.StateDir,"session-guard-events.jsonl"); default: return nil, errors.New("artifact is not allowlisted") }
	data,err:=os.ReadFile(path)
	if err==nil{return data,nil}
	if name=="heartbeat" || name=="status" { return json.Marshal(a.Status()) }
	return nil,err
}

func (a *Agent) localModeEnabled() bool {
	path:=filepath.Join(a.Config.StateDir,"local-mode.flag")
	raw,err:=os.ReadFile(path);if os.IsNotExist(err){return false};if err!=nil{return true}
	expires:=localModeExpiry(raw)
	if !expires.IsZero()&&time.Now().After(expires){if err:=os.Remove(path);err==nil||os.IsNotExist(err){return false}}
	return true
}

func localModeExpiry(raw []byte)time.Time{
	var state struct{ExpiresAt string `json:"expiresAt"`}
	if json.Unmarshal(raw,&state)==nil&&state.ExpiresAt!=""{expires,err:=time.Parse(time.RFC3339Nano,state.ExpiresAt);if err==nil{return expires};return time.Time{}}
	created,err:=time.Parse(time.RFC3339Nano,strings.TrimSpace(string(raw)));if err!=nil{return time.Time{}}
	return created.Add(8*time.Hour)
}

func (a *Agent) localMode(action string,args []string) (int,string,string,map[string]any) {
	lock,lockErr:=a.lockState();if lockErr!=nil{return 2,"","station state lock unavailable",map[string]any{"code":"STATION_STATE_LOCK_FAILED"}};defer unlockState(lock)
	path:=filepath.Join(a.Config.StateDir,"local-mode.flag")
	if action=="status" { enabled:=a.localModeEnabled();status:=map[string]any{"enabled":enabled};if enabled{if raw,err:=os.ReadFile(path);err==nil{expires:=localModeExpiry(raw);if !expires.IsZero(){status["expiresAt"]=expires.UTC().Format(time.RFC3339Nano)}}};encoded,_:=json.Marshal(status);return 0,string(encoded),"",nil }
	if err:=os.MkdirAll(a.Config.StateDir,0750); err!=nil { return 2,"",err.Error(),nil }
	if action=="clear" { if err:=os.Remove(path); err!=nil && !os.IsNotExist(err) { return 2,"",err.Error(),nil }; return 0,"local mode cleared","",nil }
	ttl:=8*time.Hour
	if len(args)>0{if !strings.HasPrefix(args[0],"--ttl="){return 2,"","local-mode accepts only --ttl=seconds",map[string]any{"code":"STATION_COMMAND_REJECTED"}};seconds,err:=strconv.Atoi(strings.TrimPrefix(args[0],"--ttl="));if err!=nil||seconds<60||seconds>86400{return 2,"","local-mode TTL must be between 60 and 86400 seconds",map[string]any{"code":"STATION_COMMAND_REJECTED"}};ttl=time.Duration(seconds)*time.Second}
	now:=time.Now().UTC();expires:=now.Add(ttl)
	if err:=atomicJSON(path,map[string]any{"enabled":true,"setAt":now.Format(time.RFC3339Nano),"expiresAt":expires.Format(time.RFC3339Nano)});err!=nil{return 2,"",err.Error(),nil}
	return 0,"local mode enabled","",map[string]any{"expiresAt":expires.Format(time.RFC3339Nano)}
}

func (a *Agent) prepare(ctx context.Context,args []string)(int,string,string,map[string]any) {
	lock,lockErr:=a.lockState();if lockErr!=nil{return 2,"","station state lock unavailable",map[string]any{"code":"STATION_STATE_LOCK_FAILED"}};defer unlockState(lock)
	a.mu.Lock(); defer a.mu.Unlock()
	guardOptions,guardErr:=parseSessionGuardOptions(args,a.Config.GuardGraceSeconds,false)
	if guardErr!=nil{return 2,"",guardErr.Error(),map[string]any{"code":"STATION_COMMAND_REJECTED"}}
	if a.Config.Profile=="fmu-only" { return 2,"","profile does not support a physical laboratory session",map[string]any{"code":"STATION_PROFILE_UNSUPPORTED"} }
	if a.localModeEnabled() { return 2,"","local mode is active",map[string]any{"code":"STATION_LOCAL_MODE_ACTIVE"} }
	local,_,sessionsOK:=a.hostRuntime().Sessions()
	if !sessionsOK { return 2,"","session inventory unavailable; refusing to prepare station",map[string]any{"code":"STATION_SESSION_QUERY_FAILED"} }
	notificationDelivered:=false
	notificationWarning:=false
	if len(local)>0 {
		if a.Config.Profile=="dedicated" { return 2,"","unexpected local session on dedicated station",map[string]any{"code":"STATION_LOCAL_SESSION_ACTIVE","sessions":local} }
		if a.Config.Profile=="hybrid" && !a.Config.AllowLocalSessionEviction { return 2,"","instructor session is active; local-session eviction is disabled",map[string]any{"code":"STATION_LOCAL_SESSION_ACTIVE","sessions":local} }
		if !guardOptions.NoGuard&&guardOptions.Notify { if err:=a.hostRuntime().NotifyLocalUsers(guardOptions.Message);err==nil{notificationDelivered=true}else{notificationWarning=true} }
		if a.Config.Profile=="hybrid" {
			if !guardOptions.NoGuard&&guardOptions.GraceSeconds>0 { if err:=waitGrace(ctx,guardOptions.GraceSeconds);err!=nil{return 2,"","session guard was cancelled",map[string]any{"code":"STATION_GUARD_CANCELLED"}} }
			for _, session:=range local { if err:=a.helper(ctx,map[string]any{"operation":"terminate-session","sessionId":session["id"]}); err!=nil { return 2,"", "unable to release instructor session",map[string]any{"code":"STATION_SESSION_TERMINATION_FAILED"} } }
		}
	}
	_ = os.MkdirAll(a.Config.StateDir,0750)
	a.appendEvent(map[string]any{"kind":"prepare-session","timestamp":time.Now().UTC().Format(time.RFC3339Nano),"sessionsReleased":len(local),"notificationDelivered":notificationDelivered})
	code:=0;message:="station prepared";if notificationWarning{code=1;message="station prepared; session notification could not be delivered"}
	return code,message,"",map[string]any{"profile":a.Config.Profile,"releasedSessions":len(local),"notificationDelivered":notificationDelivered}
}

func (a *Agent) release(ctx context.Context,args []string)(int,string,string,map[string]any) {
	lock,lockErr:=a.lockState();if lockErr!=nil{return 2,"","station state lock unavailable",map[string]any{"code":"STATION_STATE_LOCK_FAILED"}};defer unlockState(lock)
	reboot:=false;rebootTimeout:=0
	for _,arg:=range args{key,value,hasValue:=strings.Cut(arg,"=");switch key{case "--reboot":enabled,err:=booleanOptionValue(value,hasValue,true);if err!=nil{return 2,"",err.Error(),map[string]any{"code":"STATION_COMMAND_REJECTED"}};reboot=enabled;case "--reboot-timeout":if !hasValue{return 2,"","reboot timeout requires --reboot-timeout=seconds",map[string]any{"code":"STATION_COMMAND_REJECTED"}};seconds,err:=strconv.Atoi(value);if err!=nil||seconds<0||seconds>30{return 2,"","reboot timeout must be between 0 and 30 seconds",map[string]any{"code":"STATION_COMMAND_REJECTED"}};rebootTimeout=seconds}}
	local,remote,sessionsOK:=a.hostRuntime().Sessions()
	if !sessionsOK { return 2,"","session inventory unavailable; refusing to release station",map[string]any{"code":"STATION_SESSION_QUERY_FAILED"} }
	terminated:=0
	for _, session:=range remote { if session["user"]=="labuser" && session["type"]=="x11" { if err:=a.helper(ctx,map[string]any{"operation":"terminate-session","sessionId":session["id"]});err!=nil{return 2,"","unable to close Tiny Desk session",map[string]any{"code":"STATION_SESSION_TERMINATION_FAILED"}};terminated++ } }
	rebootRequested:=false
	if reboot {
		deadline:=time.Now().Add(time.Duration(rebootTimeout)*time.Second)
		for {
			local,remote,sessionsOK=a.hostRuntime().Sessions()
			if !sessionsOK{return 1,"station released; reboot deferred because session inventory is unavailable","",map[string]any{"profile":a.Config.Profile,"closedLabuserSessions":terminated,"rebootRequested":false,"reason":"session query failed"}}
			if len(local)==0&&!hasLabUserRemote(remote){break}
			if rebootTimeout==0||!time.Now().Before(deadline){return 1,"station released; reboot deferred while users are active","",map[string]any{"profile":a.Config.Profile,"closedLabuserSessions":terminated,"rebootRequested":false,"reason":"active user sessions","rebootTimeoutSeconds":rebootTimeout}}
			timer:=time.NewTimer(time.Second);select{case <-ctx.Done():timer.Stop();return 1,"station released; reboot wait was cancelled","",map[string]any{"profile":a.Config.Profile,"closedLabuserSessions":terminated,"rebootRequested":false,"reason":"reboot wait cancelled"};case <-timer.C:}
		}
		if err:=a.helper(ctx,map[string]any{"operation":"power-reboot"});err!=nil{return 2,"station released; reboot request failed","",map[string]any{"code":"STATION_POWER_FAILED","profile":a.Config.Profile,"closedLabuserSessions":terminated,"rebootRequested":false}}
		rebootRequested=true
	}
	a.appendEvent(map[string]any{"kind":"release-session","timestamp":time.Now().UTC().Format(time.RFC3339Nano),"closedLabuserSessions":terminated,"rebootRequested":rebootRequested,"rebootTimeoutSeconds":rebootTimeout})
	return 0,"station released","",map[string]any{"profile":a.Config.Profile,"closedLabuserSessions":terminated,"rebootRequested":rebootRequested,"rebootTimeoutSeconds":rebootTimeout}
}

type sessionGuardOptions struct { GraceSeconds int; Message string; Notify bool; NoGuard bool; User string }

func parseSessionGuardOptions(args []string,defaultGrace int,allowUser bool)(sessionGuardOptions,error){
	options:=sessionGuardOptions{GraceSeconds:defaultGrace,Message:"A remote laboratory reservation is starting; save your work now.",Notify:true}
	for _,arg:=range args{
		key,value,hasValue:=strings.Cut(arg,"=")
		switch key{
		case "--guard-grace","--grace":
			if !hasValue{return options,errors.New("guard grace requires --guard-grace=seconds")};seconds,err:=strconv.Atoi(value);if err!=nil||seconds<0||seconds>90{return options,errors.New("guard grace must be between 0 and 90 seconds")};options.GraceSeconds=seconds
		case "--guard-message","--message":
			if !hasValue||strings.TrimSpace(value)==""||len(value)>256{return options,errors.New("guard message must contain 1 to 256 characters")};options.Message=value
		case "--guard-notify":
			enabled,err:=booleanOptionValue(value,hasValue,true);if err!=nil{return options,err};options.Notify=enabled
		case "--guard-silent","--silent","--no-notify":
			enabled,err:=booleanOptionValue(value,hasValue,true);if err!=nil{return options,err};options.Notify=!enabled
		case "--no-guard":
			enabled,err:=booleanOptionValue(value,hasValue,true);if err!=nil{return options,err};options.NoGuard=enabled
		case "--user":
			if !allowUser{return options,errors.New("user targeting is available only for session guard")};if !hasValue||!validLocalUsername(value){return options,errors.New("session guard user is invalid")};options.User=value
		}
	}
	return options,nil
}

func booleanOptionValue(value string,hasValue,defaultValue bool)(bool,error){if !hasValue{return defaultValue,nil};if value=="true"{return true,nil};if value=="false"{return false,nil};return false,errors.New("guard boolean option must be true or false")}
func validLocalUsername(value string)bool{if len(value)<1||len(value)>64{return false};for _,char:=range value{if !((char>='a'&&char<='z')||(char>='A'&&char<='Z')||(char>='0'&&char<='9')||strings.ContainsRune("_.-",char)){return false}};return true}
func waitGrace(ctx context.Context,seconds int)error{timer:=time.NewTimer(time.Duration(seconds)*time.Second);defer timer.Stop();select{case <-ctx.Done():return ctx.Err();case <-timer.C:return nil}}

func (a *Agent) guard(ctx context.Context,args []string)(int,string,string,map[string]any) {
	options,err:=parseSessionGuardOptions(args,a.Config.GuardGraceSeconds,true);if err!=nil{return 2,"",err.Error(),map[string]any{"code":"STATION_COMMAND_REJECTED"}}
	lock,err:=a.lockState();if err!=nil{return 2,"","station state lock unavailable",map[string]any{"code":"STATION_STATE_LOCK_FAILED"}};defer unlockState(lock)
	a.mu.Lock();defer a.mu.Unlock()
	local,_,ok:=a.hostRuntime().Sessions();if !ok{return 1,"session inventory unavailable","",map[string]any{"queryOk":false}}
	selected:=filterUserSessions(local,options.User)
	notificationDelivered:=false
	if len(selected)>0&&!options.NoGuard {
		if options.Notify{if err:=a.hostRuntime().NotifyLocalUsers(options.Message);err==nil{notificationDelivered=true}}
		if options.GraceSeconds>0{if err:=waitGrace(ctx,options.GraceSeconds);err!=nil{return 2,"","session guard was cancelled",map[string]any{"code":"STATION_GUARD_CANCELLED"}}}
		local,_,ok=a.hostRuntime().Sessions();if !ok{return 1,"session inventory unavailable after guard","",map[string]any{"queryOk":false}}
		selected=filterUserSessions(local,options.User)
	}
	requestedNotification:=options.Notify&&!options.NoGuard
	a.appendEvent(map[string]any{"kind":"session-guard","timestamp":time.Now().UTC().Format(time.RFC3339Nano),"user":options.User,"graceSeconds":options.GraceSeconds,"notified":notificationDelivered,"activeSessions":len(selected)})
	code:=0;message:=fmt.Sprintf("%d local sessions remain",len(selected));if len(selected)>0||requestedNotification&&!notificationDelivered{code=1}
	return code,message,"",map[string]any{"localSessions":selected,"queryOk":true,"graceSeconds":options.GraceSeconds,"notificationRequested":requestedNotification,"notificationDelivered":notificationDelivered,"noGuard":options.NoGuard}
}
func filterUserSessions(sessions []map[string]any,user string)[]map[string]any{if user==""{return sessions};selected:=[]map[string]any{};for _,session:=range sessions{if session["user"]==user{selected=append(selected,session)}};return selected}

func (a *Agent) power(ctx context.Context,args []string)(int,string,string,map[string]any) {
	action:=args[0]
	if action=="hibernate" && !a.hostRuntime().HibernateSupported() { return 2,"","hibernate is unsupported or has no active swap target",map[string]any{"code":"STATION_POWER_UNSUPPORTED"} }
	delay:=0;reason:="requested by Lab Gateway";requireWake:=false
	for _,arg:=range args[1:]{key,value,hasValue:=strings.Cut(arg,"=");switch key{
		case "--delay":if !hasValue{return 2,"","power delay requires --delay=seconds",map[string]any{"code":"STATION_COMMAND_REJECTED"}};parsed,err:=strconv.Atoi(value);if err!=nil||parsed<0||parsed>3600{return 2,"","power delay must be between 0 and 3600 seconds",map[string]any{"code":"STATION_COMMAND_REJECTED"}};delay=parsed
		case "--reason":if !hasValue||strings.TrimSpace(value)==""||len(value)>128{return 2,"","power reason must contain 1 to 128 characters",map[string]any{"code":"STATION_COMMAND_REJECTED"}};reason=value
		case "--require-wake":requireWake=!hasValue||value=="true";if hasValue&&value!="true"&&value!="false"{return 2,"","--require-wake must be true or false",map[string]any{"code":"STATION_COMMAND_REJECTED"}}
		default:return 2,"","power option is not allowlisted",map[string]any{"code":"STATION_COMMAND_REJECTED"}
	}}
	platform:=a.hostRuntime();interfaces:=platform.NetworkInterfaces();wakeSupported,wakeEnabled:=platform.WakeStatus(interfaces);wakePersistent:=platform.WakePersistent(interfaces);if requireWake&&(!wakeSupported||!wakeEnabled||!wakePersistent){return 2,"","Wake-on-LAN is not enabled persistently on a supported interface",map[string]any{"code":"STATION_WAKE_NOT_READY"}}
	if delay>0{timer:=time.NewTimer(time.Duration(delay)*time.Second);defer timer.Stop();select{case <-ctx.Done():return 2,"","power action cancelled",map[string]any{"code":"STATION_POWER_CANCELLED"};case <-timer.C:}}
	operation:="power-"+action
	if err:=a.helper(ctx,map[string]any{"operation":operation}); err!=nil { return 2,"","privileged power operation failed",map[string]any{"code":"STATION_POWER_FAILED"} }
	warning:="";if action=="shutdown"&&(!wakeSupported||!wakeEnabled||!wakePersistent){warning="shutdown requested, but Wake-on-LAN is not ready persistently"}
	code:=0;if warning!=""{code=1}
	return code,action+" requested","",map[string]any{"action":action,"requested":true,"reason":reason,"delaySeconds":delay,"wakeReady":wakeSupported&&wakeEnabled&&wakePersistent,"warning":warning}
}

func (a *Agent) recovery(ctx context.Context,args []string)(int,string,string,map[string]any) {
	force:=false;reason:="required system update";timeoutSeconds:=0
	for _,arg:=range args{if arg=="--force"{force=true;continue};if key,value,ok:=strings.Cut(arg,"=");ok{switch key{case "--reason":if strings.TrimSpace(value)==""||len(value)>128{return 2,"","recovery reason must contain 1 to 128 characters",map[string]any{"code":"STATION_COMMAND_REJECTED"}};reason=value;case "--timeout":parsed,err:=strconv.Atoi(value);if err!=nil||parsed<0||parsed>30{return 2,"","recovery timeout must be between 0 and 30 seconds",map[string]any{"code":"STATION_COMMAND_REJECTED"}};timeoutSeconds=parsed;default:return 2,"","recovery option is not allowlisted",map[string]any{"code":"STATION_COMMAND_REJECTED"}}}else{return 2,"","recovery option requires a value",map[string]any{"code":"STATION_COMMAND_REJECTED"}}}
	if !a.rebootRequired(){return 1,"no recovery action required","",map[string]any{"rebootRequested":false,"reason":"no supported reboot-required marker is present"}}
	if a.localModeEnabled(){return 1,"recovery deferred while local mode is active","",map[string]any{"rebootRequested":false,"reason":"local mode is active"}}
	deadline:=time.Now().Add(time.Duration(timeoutSeconds)*time.Second)
	for{local,remote,sessionsOK:=a.hostRuntime().Sessions();if !sessionsOK{return 1,"recovery deferred because session inventory is unavailable","",map[string]any{"rebootRequested":false,"reason":"session query failed"}};if len(local)==0&&!hasLabUserRemote(remote){break};if timeoutSeconds==0||!time.Now().Before(deadline){return 1,"recovery deferred while laboratory users are active","",map[string]any{"rebootRequested":false,"reason":"active user sessions","timeoutSeconds":timeoutSeconds}};timer:=time.NewTimer(time.Second);select{case <-ctx.Done():timer.Stop();return 1,"recovery deferred because its wait was cancelled","",map[string]any{"rebootRequested":false,"reason":"recovery wait cancelled"};case <-timer.C:};if a.localModeEnabled(){return 1,"recovery deferred while local mode is active","",map[string]any{"rebootRequested":false,"reason":"local mode is active"}}}
	lock,err:=a.lockState();if err!=nil{return 2,"","station state lock unavailable",map[string]any{"code":"STATION_STATE_LOCK_FAILED"}};defer unlockState(lock)
	lastPath:=filepath.Join(a.Config.StateDir,"recovery-last.json");if raw,err:=os.ReadFile(lastPath);err==nil{var state map[string]string;if json.Unmarshal(raw,&state)==nil{if parsed,parseErr:=time.Parse(time.RFC3339,state["requestedAt"]);parseErr==nil&&time.Since(parsed)<24*time.Hour{return 1,"recovery deferred by the 24-hour reboot limit","",map[string]any{"rebootRequested":false,"reason":"reboot cooldown is active","forced":force}}}}
	requestedAt:=time.Now().UTC().Format(time.RFC3339Nano);if err:=atomicJSON(lastPath,map[string]any{"requestedAt":requestedAt,"reason":reason});err!=nil{return 2,"","unable to record recovery request",map[string]any{"code":"STATION_STATE_WRITE_FAILED"}}
	if err:=a.helper(ctx,map[string]any{"operation":"power-reboot"});err!=nil{_ = os.Remove(lastPath);return 2,"","privileged recovery reboot failed",map[string]any{"code":"STATION_POWER_FAILED"}}
	return 0,"recovery reboot requested","",map[string]any{"rebootRequested":true,"requestedAt":requestedAt,"reason":reason,"forced":force}
}

func (a *Agent) energyAudit() map[string]any { state:=a.hostRuntime().EnergyStatus();return map[string]any{"suspendStates":state.SuspendStates,"activeSwap":state.ActiveSwap,"hibernateSupported":state.HibernateSupported,"supervisor":state.Supervisor,"inhibitors":map[string]any{"available":state.InhibitorsAvailable,"activeCount":state.ActiveInhibitors},"automaticSuspendPolicy":state.AutomaticSuspendPolicy,"issues":state.Issues} }
func (a *Agent) powerCapabilities() map[string]any { hibernate:=a.hostRuntime().HibernateSupported();return map[string]any{"shutdown":map[string]any{"available":fileExists("/usr/lib/decentralabs/lab-station/labstation-helper"),"ready":true},"hibernate":map[string]any{"available":hibernate,"ready":hibernate}} }

func (a *Agent) service(ctx context.Context,action string)(int,string,string,map[string]any) { sup:=a.hostRuntime().Supervisor(); if sup==nil { return 2,"","service supervisor unavailable",map[string]any{"code":"STATION_SERVICE_UNAVAILABLE"} }; unit:=serviceUnit(sup.Name(),"decentralabs-labstation.service"); if action=="status" { state,err:=sup.ServiceState(unit); if err!=nil { return 1,state,err.Error(),map[string]any{"backend":sup.Name()} }; return 0,state,"",map[string]any{"backend":sup.Name()} }; if err:=a.helper(ctx,map[string]any{"operation":"service-action","unit":unit,"action":action}); err!=nil { return 2,"","service action failed",map[string]any{"backend":sup.Name()} }; return 0,"service "+action,"",map[string]any{"backend":sup.Name()} }

func (a *Agent) fmu(ctx context.Context,action string)(int,string,string,map[string]any) { sup:=a.hostRuntime().Supervisor(); if sup==nil { return 2,"","service supervisor unavailable",nil }; unit:=serviceUnit(sup.Name(),"decentralabs-labstation-fmu.service"); if action=="status" { state,err:=sup.ServiceState(unit); if err!=nil { return 1,state,err.Error(),map[string]any{"backend":sup.Name()} }; return 0,state,"",map[string]any{"backend":sup.Name()} }; if err:=a.helper(ctx,map[string]any{"operation":"service-action","unit":unit,"action":action}); err!=nil { return 2,"","FMU Executor action failed",map[string]any{"backend":sup.Name()} }; return 0,"FMU Executor "+action,"",map[string]any{"backend":sup.Name()} }

func (a *Agent) helper(ctx context.Context,request map[string]any) error { encoded,err:=json.Marshal(request); if err!=nil{return err}; if a.runHelper!=nil{return a.runHelper(ctx,encoded)}; cmd:=exec.CommandContext(ctx,"sudo","-n","/usr/lib/decentralabs/lab-station/labstation-helper"); cmd.Stdin=strings.NewReader(string(encoded)); output,err:=cmd.CombinedOutput(); if err!=nil{return fmt.Errorf("helper rejected operation: %s",strings.TrimSpace(string(output)))}; return nil }

func (a *Agent) lockState()(*os.File,error) { if err:=os.MkdirAll(a.Config.StateDir,0770);err!=nil{return nil,err};file,err:=os.OpenFile(filepath.Join(a.Config.StateDir,"station.lock"),os.O_CREATE|os.O_RDWR,0660);if err!=nil{return nil,err};if err=syscall.Flock(int(file.Fd()),syscall.LOCK_EX);err!=nil{_ = file.Close();return nil,err};return file,nil }
func unlockState(file *os.File) { if file==nil{return};_ = syscall.Flock(int(file.Fd()),syscall.LOCK_UN);_ = file.Close() }

func (a *Agent) recordOperation(result Result) { switch result.Command{case "prepare-session","release-session","power","recovery reboot-if-needed":case "local-mode","service","fmu-executor":if len(result.Options)==0||result.Options[0]=="status"{return};default:return};data:=map[string]any{"id":result.ID,"operationId":result.ID,"command":result.Command,"timestamp":result.CompletedAt,"exitCode":result.ExitCode,"outcome":result.Outcome,"message":result.Message,"profile":a.Config.Profile,"metadata":result.Metadata}; _=os.MkdirAll(a.Config.StateDir,0750); _=atomicJSON(filepath.Join(a.Config.StateDir,"service-state.json"),data) }
func (a *Agent) latestOperation() map[string]any { raw,err:=os.ReadFile(filepath.Join(a.Config.StateDir,"service-state.json")); if err!=nil{return map[string]any{}}; var value map[string]any; if json.Unmarshal(raw,&value)!=nil{return map[string]any{}}; return value }
func (a *Agent) appendEvent(value map[string]any) { _=os.MkdirAll(a.Config.StateDir,0750); file,err:=os.OpenFile(filepath.Join(a.Config.StateDir,"session-guard-events.jsonl"),os.O_CREATE|os.O_APPEND|os.O_WRONLY,0660); if err!=nil{return}; defer file.Close(); _=json.NewEncoder(file).Encode(value) }
func (a *Agent) applicationHash() string { if !a.applicationAvailable(){return ""};path,err:=filepath.EvalSymlinks(a.Config.Application.Command);if err!=nil{return ""};data,err:=os.ReadFile(path);if err!=nil{return ""};digest:=sha256.Sum256(data);return hex.EncodeToString(digest[:]) }
func (a *Agent) configurationHash()string{path:=a.Config.SourcePath;if path==""{path=os.Getenv("LABSTATION_CONFIG")};if path==""{path="/etc/decentralabs/lab-station/station.toml"};raw,err:=os.ReadFile(path);if err!=nil{return ""};hash:=sha256.New();_,_=hash.Write(raw);_,_=hash.Write([]byte{0});_,_=hash.Write([]byte(a.applicationHash()));return hex.EncodeToString(hash.Sum(nil))}
func (a *Agent) applicationAvailable() bool { path:=a.Config.Application.Command;if !strings.HasPrefix(filepath.Clean(path),"/opt/lab/apps/"){return false};root,err:=filepath.EvalSymlinks("/opt/lab/apps");if err!=nil{return false};resolved,err:=filepath.EvalSymlinks(path);if err!=nil{return false};relative,err:=filepath.Rel(root,resolved);if err!=nil||relative==".."||strings.HasPrefix(relative,".."+string(filepath.Separator)){return false};base:=strings.ToLower(filepath.Base(resolved));if oneOf(base,"sh","bash","dash","zsh","fish","env"){return false};return executable(resolved) }

func serviceState(sup host.Supervisor,unit string)string { if sup==nil{return "unknown"}; state,err:=sup.ServiceState(serviceUnit(sup.Name(),unit)); if err!=nil{return "unknown"}; return state }
func (a *Agent) tinyDeskConfigurationReady()bool {
	if !fileExists("/home/labuser/.xsession")||!executable("/usr/bin/openbox")||!executable("/usr/bin/labstationctl"){return false}
	data,err:=os.ReadFile("/home/labuser/.xsession");if err!=nil||!strings.Contains(string(data),"openbox --config-file /etc/decentralabs/lab-station/tiny-desk-rc.xml")||!strings.Contains(string(data),"exec /usr/bin/labstationctl app launch"){return false}
	policy,err:=os.ReadFile("/etc/decentralabs/lab-station/tiny-desk-rc.xml");if err!=nil||string(policy)!=tinyDeskOpenboxConfig{return false}
	ini,err:=os.ReadFile("/etc/xrdp/xrdp.ini");if err!=nil{return false};section:="";channels:=false;multimon:=false
	for _,line:=range strings.Split(string(ini),"\n"){line=strings.TrimSpace(line);if strings.HasPrefix(line,"[")&&strings.HasSuffix(line,"]"){section=strings.ToLower(strings.TrimSpace(line[1:len(line)-1]));continue};if section!="globals"||strings.HasPrefix(line,"#")||strings.HasPrefix(line,";"){continue};key,value,ok:=strings.Cut(line,"=");if !ok{continue};switch strings.ToLower(strings.TrimSpace(key)){case "allow_channels":channels=strings.EqualFold(strings.TrimSpace(value),"false");case "allow_multimon":multimon=strings.EqualFold(strings.TrimSpace(value),"false")}}
	if !channels||!multimon{return false}
	return a.hostRuntime().UserPasswordConfigured("labuser")
}
func serviceUnit(supervisor,unit string)string { if supervisor=="openrc" {return strings.TrimSuffix(unit,".service")};return unit }
func executable(path string)bool { info,err:=os.Stat(path);return err==nil && !info.IsDir() && info.Mode()&0111!=0 }
func fileExists(path string)bool {_,err:=os.Stat(path);return err==nil}
func (a *Agent) rebootRequired()bool {if a.rebootMarker!=nil{return a.rebootMarker()};return fileExists("/run/reboot-required")||fileExists("/var/run/reboot-required")}
func fileText(path string)string {raw,err:=os.ReadFile(path);if err!=nil{return ""};return strings.TrimSpace(string(raw))}
func firstValue(manager string,_ []string)string { if manager=="" {return "unknown"};return manager }
func oneOf(value string,items ...string)bool { for _,item:=range items {if value==item{return true}};return false }
func newID()string { data:=make([]byte,16); if _,err:=rand.Read(data);err!=nil{return fmt.Sprint(time.Now().UnixNano())};return hex.EncodeToString(data) }

func atomicWrite(path string,data []byte,mode os.FileMode)error { if err:=os.MkdirAll(filepath.Dir(path),0750);err!=nil{return err};temp:=path+".tmp";file,err:=os.OpenFile(temp,os.O_CREATE|os.O_TRUNC|os.O_WRONLY,mode);if err!=nil{return err};if _,err=file.Write(data);err!=nil{_ = file.Close();_ = os.Remove(temp);return err};if err=file.Sync();err!=nil{_ = file.Close();_ = os.Remove(temp);return err};if err=file.Close();err!=nil{return err};return os.Rename(temp,path) }
func atomicJSON(path string,value any)error { data,err:=json.Marshal(value);if err!=nil{return err};return atomicWrite(path,append(data,'\n'),0640) }


func capabilityIssues(ready bool,message string)[]string { if ready{return []string{}};return []string{message} }
func hasLabUserRemote(sessions []map[string]any)bool{for _,session:=range sessions{if session["user"]=="labuser"{return true}};return false}
func architecture()string { switch runtime.GOARCH {case "amd64":return "x86_64";case "arm64":return "aarch64";case "386":return "x86";case "arm":return "armv7";default:return ""} }
