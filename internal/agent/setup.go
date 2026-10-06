package agent

import (
	"errors"
	"flag"
	"fmt"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/decentralabs/lab-station-linux/internal/config"
	"github.com/decentralabs/lab-station-linux/internal/host"
)

var publicKeyPattern=regexp.MustCompile(`^ssh-ed25519 [A-Za-z0-9+/]+={0,2}( [^\r\n]{1,128})?$`)
var wakeInterfacePattern=regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)
const sharedDirectoryMode=os.FileMode(0o770)|os.ModeSetgid

func Setup(args []string)error{
	flags:=flag.NewFlagSet("setup",flag.ContinueOnError)
	profile:=flags.String("profile","dedicated","dedicated, hybrid or fmu-only")
	publicKey:=flags.String("management-public-key","","Gateway SSH Ed25519 public key")
	noInstall:=flags.Bool("no-install-deps",false,"do not install operating system packages")
	configPath:=flags.String("config","/etc/decentralabs/lab-station/station.toml","station configuration path")
	fmuSource:=flags.String("fmu-executor-source","","optional shared FMU Executor source directory")
	sshPort:=flags.Int("ssh-port",0,"SSH port already configured by OpenSSH; defaults to the detected listener")
	wakeInterface:=flags.String("wake-interface","","enable and persist Wake-on-LAN for this interface")
	if err:=flags.Parse(args);err!=nil{return err}
	if os.Geteuid()!=0{return errors.New("setup requires one administrative run as root")}
	if !oneOf(*profile,"dedicated","hybrid","fmu-only"){return errors.New("profile must be dedicated, hybrid, or fmu-only")}
	if *sshPort<0||*sshPort>65535{return errors.New("--ssh-port must be between 1 and 65535 when supplied")}
	if *wakeInterface!=""&&!wakeInterfacePattern.MatchString(*wakeInterface){return errors.New("--wake-interface must be a valid Linux interface name")}
	if host.DetectSupervisor()==nil{return errors.New("setup supports systemd and OpenRC hosts only")}
	configRoot:="/etc/decentralabs/lab-station"
	cleanConfigPath:=filepath.Clean(*configPath);relativeConfig,relativeErr:=filepath.Rel(configRoot,cleanConfigPath)
	if !filepath.IsAbs(*configPath)||relativeErr!=nil||relativeConfig=="."||relativeConfig==".."||strings.HasPrefix(relativeConfig,".."+string(filepath.Separator))||filepath.Dir(cleanConfigPath)!=configRoot||!regexp.MustCompile(`^[A-Za-z0-9_./-]+$`).MatchString(cleanConfigPath){return errors.New("--config must be a file directly under /etc/decentralabs/lab-station")}
	*configPath=cleanConfigPath
	if info,err:=os.Lstat(*configPath);err==nil&&(!info.Mode().IsRegular()||info.Mode()&os.ModeSymlink!=0){return errors.New("station config must be a regular non-symlink file")}
	if !publicKeyPattern.MatchString(strings.TrimSpace(*publicKey)){return errors.New("--management-public-key must be an ssh-ed25519 public key")}
	cfg:=config.Defaults();cfg.Name,_=os.Hostname();cfg.Profile=*profile;cfg.ManagementPublicKey=strings.TrimSpace(*publicKey)
	if existing,err:=config.Load(*configPath);err==nil{cfg=existing;cfg.Profile=*profile;cfg.ManagementPublicKey=strings.TrimSpace(*publicKey)}else if !os.IsNotExist(err){return fmt.Errorf("invalid existing station configuration: %w",err)}
	if *fmuSource!=""{if !filepath.IsAbs(*fmuSource){return errors.New("--fmu-executor-source must be an absolute directory")};if info,err:=os.Lstat(*fmuSource);err!=nil||!info.IsDir()||info.Mode()&os.ModeSymlink!=0{return errors.New("FMU Executor source must be a readable, non-symlink directory")};if !fileExists(filepath.Join(*fmuSource,"app","main.py"))||!fileExists(filepath.Join(*fmuSource,"requirements.txt"))||!fileExists(filepath.Join(*fmuSource,"VERSION")){return errors.New("FMU Executor source must contain app/main.py, requirements.txt, and VERSION")}}
	if !*noInstall {if err:=installDependencies(cfg.Profile,*fmuSource!="");err!=nil{return err}}
	if err:=ensureGroup("labstation");err!=nil{return err}
	if err:=ensureUser("labstationd","/usr/sbin/nologin","/var/lib/decentralabs/lab-station",true,true);err!=nil{return err}
	if err:=ensureUser("labstation-ops","/bin/sh","/var/lib/labstation-ops",true,false);err!=nil{return err}
	// OpenSSH rejects accounts marked with a leading !/* even when public-key
	// auth is the only permitted method. NP is an invalid password hash that
	// keeps the account eligible for the forced-key dispatcher only.
	if err:=exec.Command("usermod","--password","NP","labstation-ops").Run();err!=nil{return errors.New("unable to disable password login for the SSH management account")}
	if cfg.Profile!="fmu-only" { if err:=ensureUser("labuser","/bin/sh","/home/labuser",false,false);err!=nil{return err} }
	if err:=ensureUser("labstation-fmu","/usr/sbin/nologin","/var/lib/decentralabs/fmu-executor",true,true);err!=nil{return err}
	if err:=addToGroup("labstation-ops","labstation");err!=nil{return err}
	if err:=addToGroup("labstationd","labstation");err!=nil{return err}
	if err:=addToGroup("labstation-fmu","labstation");err!=nil{return err}
	if cfg.Profile!="fmu-only" { if err:=addToGroup("labuser","labstation-lab");err!=nil{return err} }
	if err:=installDirectoryLayout(cfg);err!=nil{return err}
	if err:=installWakePolicy(*wakeInterface);err!=nil{return err}
	if err:=installDispatcherWrapper(*configPath);err!=nil{return err}
	if err:=installAuthorizedKey(cfg.ManagementPublicKey);err!=nil{return err}
	if err:=installSshPolicy();err!=nil{return err}
	ports,err:=configuredSshPorts();if err!=nil||len(ports)==0{return errors.New("OpenSSH did not report an effective listener port")}
	selectedPort:=ports[0];if containsPort(ports,cfg.ManagementPort){selectedPort=cfg.ManagementPort};if *sshPort>0{if !containsPort(ports,*sshPort){return errors.New("--ssh-port does not match an effective OpenSSH listener")};selectedPort=*sshPort};cfg.ManagementPort=selectedPort
	if err:=writeConfig(*configPath,cfg);err!=nil{return err}
	if err:=installHelperPolicy();err!=nil{return err}
	if cfg.Profile!="fmu-only" {if err:=installTinyDeskSession(cfg);err!=nil{return err}}
	if *fmuSource!=""{if err:=installFmuExecutor(*fmuSource);err!=nil{return err}}
	if err:=installSupervisor(cfg,*configPath,*fmuSource!="");err!=nil{return err}
	if err:=startManagementServices(cfg);err!=nil{return err}
	fmt.Println("Lab Station Linux setup completed. Set the labuser RDP password through your approved credential process before exposing Tiny Desk.")
	return nil
}

func installDependencies(profile string,withFMU bool)error{
	manager,_:=host.DetectPackageManager();if manager==""{return errors.New("no supported package manager was detected; use --no-install-deps and install dependencies manually")}
	name,version:=host.DetectDistro();_ = version
	packages:=[]string{"openssh-server","ethtool","sudo"}
	if withFMU{switch manager{case "apt":packages=append(packages,"python3","python3-venv");case "dnf","yum","zypper","pacman":packages=append(packages,"python3")}}
	if profile!="fmu-only" { switch manager {
	case "apt": if name=="debian"||name=="ubuntu"{packages=append(packages,"xrdp","xorgxrdp","xserver-xorg-core","openbox")}
	case "dnf","yum": packages=append(packages,"xrdp","xorgxrdp","xorg-x11-server-Xorg","openbox")
	case "zypper": packages=append(packages,"xrdp","xorgxrdp","xorg-x11-server","openbox")
	case "pacman": packages=append(packages,"xrdp","xorg-server","openbox")
	} }
	var command string;var args []string
	switch manager {case "apt": command="apt-get";args=append([]string{"install","-y"},packages...);case "dnf","yum":command=manager;args=append([]string{"install","-y"},packages...);case "zypper":command="zypper";args=append([]string{"--non-interactive","install","--no-recommends"},packages...);case "pacman":command="pacman";args=append([]string{"-S","--noconfirm","--needed"},packages...)}
	cmd:=exec.Command(command,args...);cmd.Stdout=os.Stdout;cmd.Stderr=os.Stderr
	if err:=cmd.Run();err!=nil{return fmt.Errorf("package dependency installation failed; rerun with --no-install-deps after resolving packages: %w",err)}
	return nil
}

func installFmuExecutor(source string)error{
	root:="/opt/decentralabs/fmu-executor"
	if err:=copyExecutorTree(filepath.Join(source,"app"),filepath.Join(root,"app"));err!=nil{return err}
	requirements,err:=os.ReadFile(filepath.Join(source,"requirements.txt"));if err!=nil{return err}
	if err:=atomicWrite(filepath.Join(root,"requirements.txt"),requirements,0644);err!=nil{return err}
	version,err:=os.ReadFile(filepath.Join(source,"VERSION"));if err!=nil{return err};if strings.TrimSpace(string(version))==""{return errors.New("shared FMU Executor version is empty")};if err:=atomicWrite(filepath.Join(root,"VERSION"),version,0644);err!=nil{return err}
	for _,path:=range []string{root,filepath.Join(root,"app")}{if err:=os.Chown(path,0,0);err!=nil{return err};if err:=os.Chmod(path,0755);err!=nil{return err}}
	venv:=filepath.Join(root,".venv")
	create:=exec.Command("python3","-m","venv","--clear",venv);create.Stdout=os.Stdout;create.Stderr=os.Stderr;if err:=create.Run();err!=nil{return fmt.Errorf("FMU Executor virtual environment could not be created: %w",err)}
	pip:=exec.Command(filepath.Join(venv,"bin","python"),"-m","pip","install","--disable-pip-version-check","--no-input","-r",filepath.Join(root,"requirements.txt"));pip.Stdout=os.Stdout;pip.Stderr=os.Stderr;if err:=pip.Run();err!=nil{return fmt.Errorf("FMU Executor dependencies could not be installed; configure a reachable package mirror and retry: %w",err)}
	state:="/var/lib/decentralabs/fmu-executor/fmu-data";if err:=os.MkdirAll(state,0o770);err!=nil{return err};uid,_:=lookupUser("labstation-fmu");if err:=os.Chown("/var/lib/decentralabs/fmu-executor",uid,lookupGroup("labstation"));err!=nil{return err};if err:=os.Chown(state,uid,lookupGroup("labstation"));err!=nil{return err};if err:=os.Chmod("/var/lib/decentralabs/fmu-executor",sharedDirectoryMode);err!=nil{return err};if err:=os.Chmod(state,sharedDirectoryMode);err!=nil{return err}
	return nil
}

func copyExecutorTree(source,destination string)error{
	return filepath.WalkDir(source,func(path string,entry os.DirEntry,walkErr error)error{
		if walkErr!=nil{return walkErr};info,err:=entry.Info();if err!=nil{return err};if info.Mode()&os.ModeSymlink!=0{return errors.New("FMU Executor source must not contain symbolic links")}
		relative,err:=filepath.Rel(source,path);if err!=nil{return err};target:=destination;if relative!="."{target=filepath.Join(destination,relative)}
		if entry.IsDir(){if err:=os.MkdirAll(target,0755);err!=nil{return err};return os.Chown(target,0,0)}
		if !info.Mode().IsRegular(){return errors.New("FMU Executor source may contain only regular files and directories")}
		input,err:=os.Open(path);if err!=nil{return err};data,readErr:=io.ReadAll(input);closeErr:=input.Close();if readErr!=nil{return readErr};if closeErr!=nil{return closeErr};if err:=atomicWrite(target,data,0644);err!=nil{return err};return os.Chown(target,0,0)
	})
}

func ensureGroup(name string)error{if exec.Command("getent","group",name).Run()==nil{return nil};return exec.Command("groupadd","--system",name).Run()}
func ensureUser(name,shell,home string,system,lockPassword bool)error{
	created:=exec.Command("id","-u",name).Run()!=nil
	if created{
		args:=[]string{"--create-home","--home-dir",home,"--shell",shell}
		if system{args=append([]string{"--system"},args...)}
		args=append(args,name)
		if err:=exec.Command("useradd",args...).Run();err!=nil{return fmt.Errorf("unable to create account %s: %w",name,err)}
	}
	if lockPassword && (created || name!="labuser") { _ = exec.Command("passwd","--lock",name).Run() }
	return nil
}
func addToGroup(user,group string)error{if exec.Command("getent","group",group).Run()!=nil{if err:=exec.Command("groupadd","--system",group).Run();err!=nil{return err}};return exec.Command("usermod","--append","--groups",group,user).Run()}

func writeConfig(path string,cfg config.Config)error{
	if err:=os.MkdirAll(filepath.Dir(path),0750);err!=nil{return err}
	if err:=os.Chown(filepath.Dir(path),0,lookupGroup("labstation"));err!=nil{return err}
	if err:=os.Chmod(filepath.Dir(path),0750);err!=nil{return err}
	args:=make([]string,len(cfg.Application.Args));for i,value:=range cfg.Application.Args{args[i]=strconv.Quote(value)}
	content:=fmt.Sprintf("[station]\nname = %s\nprofile = %s\nversion = %s\nstate_dir = %s\nconfig_dir = %s\nlog_dir = %s\nmanagement_user = %s\nmanagement_port = %d\nmanagement_public_key = %s\ntransport = %s\nsupervisor = %s\nguard_grace_seconds = %d\nallow_local_session_eviction = %t\n\n[application]\nid = %s\ncommand = %s\nargs = [%s]\nuser = %s\nclose_timeout_seconds = %d\n",strconv.Quote(cfg.Name),strconv.Quote(cfg.Profile),strconv.Quote(cfg.Version),strconv.Quote(cfg.StateDir),strconv.Quote(cfg.ConfigDir),strconv.Quote(cfg.LogDir),strconv.Quote(cfg.ManagementUser),cfg.ManagementPort,strconv.Quote(cfg.ManagementPublicKey),strconv.Quote(cfg.Transport),strconv.Quote(cfg.Supervisor),cfg.GuardGraceSeconds,cfg.AllowLocalSessionEviction,strconv.Quote(cfg.Application.ID),strconv.Quote(cfg.Application.Command),strings.Join(args,", "),strconv.Quote(cfg.Application.User),cfg.Application.CloseTimeoutSeconds)
	if err:=atomicWrite(path,[]byte(content),0640);err!=nil{return err};return os.Chown(path,0,lookupGroup("labstation"))
}

func installDirectoryLayout(cfg config.Config)error{
	shared:=[]string{cfg.StateDir,filepath.Join(cfg.StateDir,"commands"),filepath.Join(cfg.StateDir,"commands","inbox"),filepath.Join(cfg.StateDir,"commands","processing"),filepath.Join(cfg.StateDir,"commands","processed"),filepath.Join(cfg.StateDir,"commands","results"),cfg.LogDir}
	for _,path:=range shared{if err:=os.MkdirAll(path,0o770);err!=nil{return err};if err:=os.Chown(path,0,lookupGroup("labstation"));err!=nil{return err};if err:=os.Chmod(path,sharedDirectoryMode);err!=nil{return err}}
	events:=filepath.Join(cfg.StateDir,"session-guard-events.jsonl");if _,err:=os.Stat(events);os.IsNotExist(err){if err:=atomicWrite(events,[]byte{},0660);err!=nil{return err}}else if err!=nil{return err};if err:=os.Chown(events,0,lookupGroup("labstation"));err!=nil{return err};if err:=os.Chmod(events,0660);err!=nil{return err}
	for _,path:=range []string{"/etc/decentralabs/lab-station"}{if err:=os.MkdirAll(path,0750);err!=nil{return err};if err:=os.Chown(path,0,lookupGroup("labstation"));err!=nil{return err};if err:=os.Chmod(path,0750);err!=nil{return err}}
	for _,path:=range []string{"/etc/decentralabs/lab-station/ssh","/etc/decentralabs/lab-station/secrets"}{if err:=os.MkdirAll(path,0700);err!=nil{return err};if err:=os.Chown(path,0,0);err!=nil{return err};if err:=os.Chmod(path,0700);err!=nil{return err}}
	if err:=os.MkdirAll("/usr/lib/decentralabs/lab-station",0755);err!=nil{return err};if err:=os.Chown("/usr/lib/decentralabs/lab-station",0,0);err!=nil{return err};if err:=os.Chmod("/usr/lib/decentralabs/lab-station",0755);err!=nil{return err}
	return nil
}

func installAuthorizedKey(key string)error{
	if !publicKeyPattern.MatchString(key){return errors.New("invalid management public key")}
	home:="/var/lib/labstation-ops";sshDir:=filepath.Join(home,".ssh");if err:=os.MkdirAll(sshDir,0700);err!=nil{return err}
	line:=`restrict,command="/usr/lib/decentralabs/lab-station/labstation-dispatcher" `+key+"\n"
	if err:=atomicWrite(filepath.Join(sshDir,"authorized_keys"),[]byte(line),0600);err!=nil{return err}
	uid,gid:=lookupUser("labstation-ops");_ = os.Chown(sshDir,uid,gid);_ = os.Chown(filepath.Join(sshDir,"authorized_keys"),uid,gid)
	return nil
}

func installDispatcherWrapper(configPath string)error{
	if !filepath.IsAbs(configPath)||strings.ContainsAny(configPath,"\r\n\x00"){return errors.New("station config path must be an absolute path without control characters")}
	quoted:=strings.ReplaceAll(configPath,"'","'\\''")
	content:="#!/bin/sh\nexport LABSTATION_CONFIG='"+quoted+"'\nexec /usr/lib/decentralabs/lab-station/labstation-dispatcher-bin\n"
	path:="/usr/lib/decentralabs/lab-station/labstation-dispatcher"
	if err:=atomicWrite(path,[]byte(content),0755);err!=nil{return err}
	return os.Chmod(path,0755)
}

func installSshPolicy()error{
	if _,err:=exec.LookPath("sshd");err!=nil{return errors.New("OpenSSH server executable sshd was not found")}
	path:="/etc/ssh/sshd_config"
	block:="# BEGIN DecentraLabs Lab Station policy\nMatch User labstation-ops\n    AuthenticationMethods publickey\n    PasswordAuthentication no\n    KbdInteractiveAuthentication no\n    PermitUserRC no\n    PermitUserEnvironment no\n    ForceCommand /usr/lib/decentralabs/lab-station/labstation-dispatcher\n    AllowTcpForwarding no\n    AllowAgentForwarding no\n    X11Forwarding no\n    PermitTTY no\n    PermitTunnel no\n# END DecentraLabs Lab Station policy\n"
	original,err:=os.ReadFile(path);if err!=nil{return errors.New("OpenSSH main configuration is unavailable")}
	backup:=path+".decentralabs-lab-station.bak";marker:=path+".decentralabs-lab-station.sha256";modified:=false
	if previous,readErr:=os.ReadFile(marker);readErr==nil{digest:=sha256.Sum256(original);if strings.TrimSpace(string(previous))!=hex.EncodeToString(digest[:]){return errors.New("sshd_config changed after Lab Station configured it; review the file and backup before retrying setup")}}else if !os.IsNotExist(readErr){return readErr}else{
		if strings.Contains(string(original),"# BEGIN DecentraLabs Lab Station policy"){return errors.New("sshd_config contains an untracked Lab Station policy; review it before retrying setup")}
		if _,statErr:=os.Stat(backup);os.IsNotExist(statErr){info,infoErr:=os.Stat(path);if infoErr!=nil{return infoErr};if err:=atomicWrite(backup,original,info.Mode().Perm());err!=nil{return err};_ = os.Chown(backup,0,0)}else if statErr!=nil{return statErr}
		updated:=append([]byte(nil),original...);if len(updated)>0&&updated[len(updated)-1]!='\n'{updated=append(updated,'\n')};updated=append(updated,[]byte("\n"+block)...);info,infoErr:=os.Stat(path);if infoErr!=nil{return infoErr};if err:=atomicWrite(path,updated,info.Mode().Perm());err!=nil{return err};if err:=os.Chown(path,0,0);err!=nil{return err};modified=true;digest:=sha256.Sum256(updated);if err:=atomicWrite(marker,[]byte(hex.EncodeToString(digest[:])+"\n"),0600);err!=nil{_ = atomicWrite(path,original,info.Mode().Perm());return err}
	}
	rollback:=func(){if !modified{return};backupData,backupErr:=os.ReadFile(backup);if backupErr!=nil{return};info,infoErr:=os.Stat(backup);if infoErr==nil{_ = atomicWrite(path,backupData,info.Mode().Perm());_ = os.Remove(marker)}}
	if err:=exec.Command("sshd","-t").Run();err!=nil{rollback();return errors.New("OpenSSH rejected the Lab Station user policy")}
	out,err:=exec.Command("sshd","-T","-C","user=labstation-ops,host=localhost,addr=127.0.0.1").Output();if err!=nil{rollback();return errors.New("OpenSSH effective configuration could not be inspected")}
	settings:=strings.ToLower(string(out))
	for _,required:=range []string{"authenticationmethods publickey","passwordauthentication no","kbdinteractiveauthentication no","permituserrc no","permituserenvironment no","forcecommand /usr/lib/decentralabs/lab-station/labstation-dispatcher","allowtcpforwarding no","allowagentforwarding no","x11forwarding no","permittty no","permittunnel no"}{if !strings.Contains(settings,required){rollback();return errors.New("OpenSSH did not apply the restricted Station management policy")}}
	return nil
}

func configuredSshPorts()([]int,error){out,err:=exec.Command("sshd","-T").Output();if err!=nil{return nil,err};ports:=[]int{};for _,line:=range strings.Split(string(out),"\n"){fields:=strings.Fields(line);if len(fields)<2||fields[0]!="port"{continue};port,err:=strconv.Atoi(fields[1]);if err!=nil||port<1||port>65535{continue};if !containsPort(ports,port){ports=append(ports,port)}};return ports,nil}
func containsPort(ports []int,wanted int)bool{for _,port:=range ports{if port==wanted{return true}};return false}

func installHelperPolicy()error{
	helperWrapper:="#!/bin/sh\nexec /usr/lib/decentralabs/lab-station/labstation-helper-bin\n"
	if err:=atomicWrite("/usr/lib/decentralabs/lab-station/labstation-helper",[]byte(helperWrapper),0755);err!=nil{return err}
	if err:=os.Chown("/usr/lib/decentralabs/lab-station/labstation-helper",0,0);err!=nil{return err}
	path:="/etc/sudoers.d/decentralabs-lab-station"
	content:="labstation-ops ALL=(root) NOPASSWD: /usr/lib/decentralabs/lab-station/labstation-helper\n"
	if err:=atomicWrite(path,[]byte(content),0440);err!=nil{return err};_ = os.Chown(path,0,0);_ = os.Chmod(path,0440)
	if _,err:=exec.LookPath("visudo");err==nil{if err:=exec.Command("visudo","-cf",path).Run();err!=nil{_ = os.Remove(path);return errors.New("sudoers policy failed visudo validation")}}
	return nil
}

const wakeInterfaceConfigPath="/etc/decentralabs/lab-station/wake-interfaces"
const wakeOriginalConfigPath="/etc/decentralabs/lab-station/wake-original"
const wakeApplyScriptPath="/usr/lib/decentralabs/lab-station/labstation-wol"

func installWakePolicy(requested string)error{
	if requested!=""{
		if !wakeInterfacePattern.MatchString(requested){return errors.New("invalid Wake-on-LAN interface name")}
		if existing,err:=os.ReadFile(wakeInterfaceConfigPath);err==nil&&string(existing)!=requested+"\n"{return errors.New("Wake-on-LAN interface changed since setup; review the managed config before retrying") }else if err!=nil&&!os.IsNotExist(err){return err}
		if _,err:=os.Stat(wakeOriginalConfigPath);os.IsNotExist(err){mode,supported,readErr:=currentWakeMode(requested);if readErr!=nil{return readErr};if !supported{return errors.New("selected interface does not support Wake-on-LAN magic packets")};if err:=writeManagedConfig(wakeOriginalConfigPath,requested+" "+mode+"\n",0600);err!=nil{return err}}else if err!=nil{return err}
		if err:=writeManagedConfig(wakeInterfaceConfigPath,requested+"\n",0600);err!=nil{return err}
	}
	info,err:=os.Lstat(wakeInterfaceConfigPath);if os.IsNotExist(err){return nil};if err!=nil{return err};if !info.Mode().IsRegular()||info.Mode()&os.ModeSymlink!=0{return errors.New("Wake-on-LAN interface config must be a regular non-symlink file")}
	raw,err:=os.ReadFile(wakeInterfaceConfigPath);if err!=nil{return err};fields:=strings.Fields(string(raw));if len(fields)!=1||!wakeInterfacePattern.MatchString(fields[0]){return errors.New("Wake-on-LAN config must contain exactly one valid interface name")}
	iface:=fields[0]
	if err:=writeManagedConfig(wakeInterfaceConfigPath,string(raw),0600);err!=nil{return err}
	if _,err:=os.Stat(wakeOriginalConfigPath);os.IsNotExist(err){mode,supported,readErr:=currentWakeMode(iface);if readErr!=nil{return readErr};if !supported{return errors.New("configured interface does not support Wake-on-LAN magic packets")};if err:=writeManagedConfig(wakeOriginalConfigPath,iface+" "+mode+"\n",0600);err!=nil{return err}}else if err!=nil{return err}
	original,err:=os.ReadFile(wakeOriginalConfigPath);if err!=nil{return err};if err:=writeManagedConfig(wakeOriginalConfigPath,string(original),0600);err!=nil{return err}
	originalFields:=strings.Fields(string(original));if len(originalFields)!=2||originalFields[0]!=iface||!regexp.MustCompile(`^[a-z]+$`).MatchString(originalFields[1]){return errors.New("Wake-on-LAN restore state is invalid; review it before retrying setup")}
	mode,supported,err:=currentWakeMode(iface);if err!=nil{return err};if !supported{return errors.New("configured interface no longer supports Wake-on-LAN magic packets")}
	if mode!="g"{if err:=exec.Command("ethtool","--change",iface,"wol","g").Run();err!=nil{return fmt.Errorf("Wake-on-LAN could not be enabled on %s: %w",iface,err)};mode,supported,err=currentWakeMode(iface);if err!=nil||!supported||mode!="g"{return errors.New("Wake-on-LAN did not remain enabled on the configured interface")}}
	script:="#!/bin/sh\nset -eu\nwhile IFS= read -r iface; do\n    [ -n \"$iface\" ] || continue\n    case \"$iface\" in *[!A-Za-z0-9_.:-]*|'') echo 'invalid configured network interface' >&2; exit 2 ;; esac\n    ethtool --change \"$iface\" wol g\ndone < /etc/decentralabs/lab-station/wake-interfaces\n"
	return writeManagedConfig(wakeApplyScriptPath,script,0755)
}

func currentWakeMode(iface string)(string,bool,error){
	if !wakeInterfacePattern.MatchString(iface){return "",false,errors.New("invalid Wake-on-LAN interface name")}
	out,err:=exec.Command("ethtool","--show-wol",iface).CombinedOutput();if err!=nil{return "",false,fmt.Errorf("Wake-on-LAN state for %s could not be inspected",iface)}
	mode:="";supported:=false
	for _,line:=range strings.Split(string(out),"\n"){line=strings.TrimSpace(line);if strings.HasPrefix(line,"Supports Wake-on:"){supported=strings.Contains(strings.TrimSpace(strings.TrimPrefix(line,"Supports Wake-on:")),"g")};if strings.HasPrefix(line,"Wake-on:"){values:=strings.Fields(line);if len(values)>1{mode=values[1]}}}
	if mode==""{return "",supported,errors.New("Wake-on-LAN driver did not report its current mode")}
	return mode,supported,nil
}

func installWakeSupervisor()error{
	if !fileExists(wakeInterfaceConfigPath){return nil}
	if sup:=host.DetectSupervisor();sup!=nil&&sup.Name()=="systemd"{
		unit:="[Unit]\nDescription=DecentraLabs Lab Station Wake-on-LAN persistence\nAfter=network.target\n\n[Service]\nType=oneshot\nExecStart="+wakeApplyScriptPath+"\nRemainAfterExit=yes\n\n[Install]\nWantedBy=multi-user.target\n"
		return writeManagedConfig("/etc/systemd/system/decentralabs-labstation-wol.service",unit,0644)
	}
	if sup:=host.DetectSupervisor();sup!=nil&&sup.Name()=="openrc"{
		script:="#!/sbin/openrc-run\ndescription=\"DecentraLabs Lab Station Wake-on-LAN persistence\"\ndepend() { need net; }\nstart() { ebegin \"Applying Lab Station Wake-on-LAN\"; "+wakeApplyScriptPath+"; eend $?; }\nstop() { return 0; }\n"
		return writeManagedConfig("/etc/init.d/decentralabs-labstation-wol",script,0755)
	}
	return errors.New("Wake-on-LAN persistence requires systemd or OpenRC")
}

func installTinyDeskSession(cfg config.Config)error{
	if cfg.Application.Command!="" && !strings.HasPrefix(filepath.Clean(cfg.Application.Command),"/opt/lab/apps/"){return errors.New("Tiny Desk application command must be under /opt/lab/apps")}
	if err:=configureTinyDeskRdp();err!=nil{return err}
	openboxConfigPath:="/etc/decentralabs/lab-station/tiny-desk-rc.xml"
	if err:=writeManagedConfig(openboxConfigPath,tinyDeskOpenboxConfig,0644);err!=nil{return err}
	home:="/home/labuser";if err:=os.MkdirAll(home,0750);err!=nil{return err}
	sessionPath:=filepath.Join(home,".xsession")
	content:=[]byte("#!/bin/sh\n/usr/bin/openbox --config-file /etc/decentralabs/lab-station/tiny-desk-rc.xml &\nexec /usr/bin/labstationctl app launch\n")
	if existing,err:=os.ReadFile(sessionPath);err==nil { if string(existing)!=string(content){return errors.New("/home/labuser/.xsession already exists; review it before enabling Tiny Desk") } } else if !os.IsNotExist(err){return err} else if err:=atomicWrite(sessionPath,content,0750);err!=nil{return err}
	uid,gid:=lookupUser("labuser");if err:=os.Chown(sessionPath,uid,gid);err!=nil{return err}
	return nil
}

// The dedicated rc.xml omits every key and mouse binding. Openbox's default
// configuration exposes a root menu and desktop shortcuts, so Tiny Desk uses
// this explicit configuration and launches the one approved lab application.
const tinyDeskOpenboxConfig=`<?xml version="1.0" encoding="UTF-8"?>
<openbox_config xmlns="http://openbox.org/3.4/rc">
  <desktops>
    <number>1</number>
    <firstdesk>1</firstdesk>
  </desktops>
  <keyboard/>
  <mouse/>
  <applications>
    <application class="*">
      <decor>no</decor>
    </application>
  </applications>
</openbox_config>
`

func writeManagedConfig(path,content string,mode os.FileMode)error{
	expected:=[]byte(content)
	digest:=sha256.Sum256(expected)
	digestText:=hex.EncodeToString(digest[:])
	marker:=path+".sha256"
	if info,err:=os.Lstat(path);err==nil&&(!info.Mode().IsRegular()||info.Mode()&os.ModeSymlink!=0){return fmt.Errorf("%s must be a regular non-symlink file",path)}else if err!=nil&&!os.IsNotExist(err){return err}
	if info,err:=os.Lstat(marker);err==nil&&(!info.Mode().IsRegular()||info.Mode()&os.ModeSymlink!=0){return fmt.Errorf("%s ownership marker must be a regular non-symlink file",marker)}else if err!=nil&&!os.IsNotExist(err){return err}
	previous,markerErr:=os.ReadFile(marker)
	if markerErr!=nil&&!os.IsNotExist(markerErr){return markerErr}
	if markerErr==nil&&strings.TrimSpace(string(previous))!=digestText{return fmt.Errorf("%s ownership marker does not match; review it before retrying setup",path)}
	if existing,err:=os.ReadFile(path);err==nil{
		if string(existing)!=string(expected){return fmt.Errorf("%s was changed outside Lab Station; review it before retrying setup",path)}
	}else if !os.IsNotExist(err){return err}else{
		if err:=atomicWrite(path,expected,mode);err!=nil{return err}
		if err:=os.Chown(path,0,0);err!=nil{return err}
		if err:=os.Chmod(path,mode);err!=nil{return err}
	}
	if markerErr==nil{return nil}
	return atomicWrite(marker,[]byte(digestText+"\n"),0600)
}

// xrdp is a host-wide listener. Tiny Desk disables channel redirection so the
// laboratory login cannot mount client drives or forward devices into a lab.
func configureTinyDeskRdp()error{
	path:="/etc/xrdp/xrdp.ini"
	original,err:=os.ReadFile(path);if err!=nil{return errors.New("xrdp.ini is unavailable; install xrdp before enabling a graphical profile")}
	backup:=path+".decentralabs-lab-station.bak"
	if _,err:=os.Stat(backup);os.IsNotExist(err){if err:=atomicWrite(backup,original,0600);err!=nil{return err};_ = os.Chown(backup,0,0)}else if err!=nil{return err}
	marker:=path+".decentralabs-lab-station.sha256"
	if previous,readErr:=os.ReadFile(marker);readErr==nil{digest:=sha256.Sum256(original);if strings.TrimSpace(string(previous))!=hex.EncodeToString(digest[:]){return errors.New("xrdp.ini changed after Lab Station configured it; review the file and backup before retrying setup")}}else if !os.IsNotExist(readErr){return readErr}
	lines:=strings.Split(string(original),"\n")
	section:="";seenChannels:=false;seenMultimon:=false;foundGlobals:=false;output:=make([]string,0,len(lines)+3)
	for _,line:=range lines{
		trimmed:=strings.TrimSpace(line)
		if strings.HasPrefix(trimmed,"[")&&strings.HasSuffix(trimmed,"]"){
			if strings.EqualFold(section,"Globals"){if !seenChannels{output=append(output,"allow_channels=false")};if !seenMultimon{output=append(output,"allow_multimon=false")}}
			section=strings.TrimSpace(trimmed[1:len(trimmed)-1]);if strings.EqualFold(section,"Globals"){foundGlobals=true}
			output=append(output,line);continue
		}
		if strings.EqualFold(section,"Globals")&&!strings.HasPrefix(trimmed,"#")&&!strings.HasPrefix(trimmed,";"){
			key,_,ok:=strings.Cut(trimmed,"=");if ok{switch strings.ToLower(strings.TrimSpace(key)){case "allow_channels":line="allow_channels=false";seenChannels=true;case "allow_multimon":line="allow_multimon=false";seenMultimon=true}}
		}
		output=append(output,line)
	}
	if !foundGlobals{return errors.New("xrdp.ini has no [Globals] section; refusing to enable an unisolated Tiny Desk listener")}
	if strings.EqualFold(section,"Globals"){if !seenChannels{output=append(output,"allow_channels=false")};if !seenMultimon{output=append(output,"allow_multimon=false")}}
	updated:=[]byte(strings.Join(output,"\n"));if err:=atomicWrite(path,updated,0644);err!=nil{return err};if err:=os.Chown(path,0,0);err!=nil{return err};digest:=sha256.Sum256(updated);return atomicWrite(marker,[]byte(hex.EncodeToString(digest[:])+"\n"),0600)
}

func installSupervisor(cfg config.Config,configPath string,withFMU bool)error{
	if sup:=host.DetectSupervisor();sup!=nil&&sup.Name()=="systemd"{
		unit:=`[Unit]
Description=DecentraLabs Lab Station daemon
After=network.target

[Service]
Type=simple
User=labstationd
Group=labstation
Environment=LABSTATION_CONFIG=`+configPath+`
ExecStart=/usr/bin/labstationd
Restart=on-failure
RestartSec=5
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
LockPersonality=true
MemoryDenyWriteExecute=true
ReadWritePaths=/var/lib/decentralabs/lab-station/data /var/log/decentralabs/lab-station /etc/decentralabs/lab-station/secrets

[Install]
WantedBy=multi-user.target
`
		unit=strings.ReplaceAll(unit,"ReadWritePaths=/var/lib/decentralabs/lab-station/data /var/log/decentralabs/lab-station /etc/decentralabs/lab-station/secrets","ReadWritePaths="+cfg.StateDir+" "+cfg.LogDir+" /etc/decentralabs/lab-station/secrets")
		fmu:=`[Unit]
Description=DecentraLabs FMU Executor
After=network.target

[Service]
Type=simple
User=labstation-fmu
Group=labstation
EnvironmentFile=-/etc/decentralabs/lab-station/secrets/fmu-internal-token.env
Environment=FMU_ROOT=/var/lib/decentralabs/fmu-executor/fmu-data
WorkingDirectory=/opt/decentralabs/fmu-executor
ExecStart=/opt/decentralabs/fmu-executor/.venv/bin/python -m app
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=/var/lib/decentralabs/fmu-executor /opt/decentralabs/fmu-executor/fmu-data

[Install]
WantedBy=multi-user.target
`
		if err:=atomicWrite("/etc/systemd/system/decentralabs-labstation.service",[]byte(unit),0644);err!=nil{return err}
		if withFMU{if err:=atomicWrite("/etc/systemd/system/decentralabs-labstation-fmu.service",[]byte(fmu),0644);err!=nil{return err}}
		if err:=installWakeSupervisor();err!=nil{return err}
		return exec.Command("systemctl","daemon-reload").Run()
	}
	if sup:=host.DetectSupervisor();sup!=nil&&sup.Name()=="openrc"{
		script:="#!/sbin/openrc-run\nexport LABSTATION_CONFIG='"+configPath+"'\ncommand=/usr/bin/labstationd\ncommand_args=\"\"\ncommand_user=labstationd:labstation\ncommand_background=true\npidfile=/run/${RC_SVCNAME}.pid\noutput_log="+cfg.LogDir+"/labstation.log\nerror_log="+cfg.LogDir+"/labstation.log\n"
		if err:=os.WriteFile("/etc/init.d/decentralabs-labstation",[]byte(script),0755);err!=nil{return err}
		if err:=os.Chmod("/etc/init.d/decentralabs-labstation",0755);err!=nil{return err}
		if withFMU{fmuScript:="#!/sbin/openrc-run\ncommand=/opt/decentralabs/fmu-executor/.venv/bin/python\ncommand_args=\"-m app\"\ncommand_user=labstation-fmu:labstation\ncommand_background=true\npidfile=/run/${RC_SVCNAME}.pid\noutput_log=/var/log/decentralabs/lab-station/fmu-executor.log\nerror_log=/var/log/decentralabs/lab-station/fmu-executor.log\nstart_pre() { [ -r /etc/decentralabs/lab-station/secrets/fmu-internal-token.env ] || return 1; . /etc/decentralabs/lab-station/secrets/fmu-internal-token.env; export FMU_INTERNAL_TOKEN_B64; export FMU_ROOT=/var/lib/decentralabs/fmu-executor/fmu-data; }\n"
			if err:=os.WriteFile("/etc/init.d/decentralabs-labstation-fmu",[]byte(fmuScript),0755);err!=nil{return err};if err:=os.Chmod("/etc/init.d/decentralabs-labstation-fmu",0755);err!=nil{return err}}
		return installWakeSupervisor()
	}
	return errors.New("no supported service supervisor was detected")
}

func startManagementServices(cfg config.Config)error{
	sup:=host.DetectSupervisor();if sup==nil{return errors.New("service supervisor unavailable after setup")}
	if sup.Name()=="systemd"{
		sshStarted:=false
		for _,unit:=range []string{"ssh.service","sshd.service"}{if exec.Command("systemctl","enable","--now",unit).Run()==nil{sshStarted=true;break}}
		if !sshStarted{return errors.New("SSH server could not be enabled")}
		for _,unit:=range []string{"ssh.service","sshd.service"}{if exec.Command("systemctl","reload",unit).Run()==nil{break}}
		if fileExists(wakeInterfaceConfigPath){if err:=exec.Command("systemctl","enable","--now","decentralabs-labstation-wol.service").Run();err!=nil{return errors.New("Wake-on-LAN persistence service could not be enabled")}}
		if err:=exec.Command("systemctl","enable","--now","decentralabs-labstation.service").Run();err!=nil{return fmt.Errorf("labstationd could not be enabled: %w",err)}
		if cfg.Profile!="fmu-only" {started:=false;for _,unit:=range []string{"xrdp.service","xrdp"}{if exec.Command("systemctl","enable","--now",unit).Run()==nil{started=true;break}};if !started{return errors.New("xrdp service could not be enabled")};restarted:=false;for _,unit:=range []string{"xrdp.service","xrdp"}{if exec.Command("systemctl","restart",unit).Run()==nil{restarted=true;break}};if !restarted{return errors.New("xrdp service did not accept the Tiny Desk security configuration")}}
		return nil
	}
	if sup.Name()=="openrc"{
		if err:=exec.Command("rc-update","add","sshd","default").Run();err!=nil{return err}
		if err:=exec.Command("rc-service","sshd","start").Run();err!=nil{return err}
		if err:=exec.Command("rc-service","sshd","restart").Run();err!=nil{return err}
		if fileExists(wakeInterfaceConfigPath){if err:=exec.Command("rc-update","add","decentralabs-labstation-wol","default").Run();err!=nil{return err};if err:=exec.Command("rc-service","decentralabs-labstation-wol","start").Run();err!=nil{return errors.New("Wake-on-LAN persistence service could not be started")}}
		if err:=exec.Command("rc-update","add","decentralabs-labstation","default").Run();err!=nil{return err}
		if err:=exec.Command("rc-service","decentralabs-labstation","start").Run();err!=nil{return err}
		if cfg.Profile!="fmu-only"{if err:=exec.Command("rc-update","add","xrdp","default").Run();err!=nil{return err};if exec.Command("rc-service","xrdp","start").Run()!=nil{if err:=exec.Command("rc-service","xrdp","restart").Run();err!=nil{return err}}else if err:=exec.Command("rc-service","xrdp","restart").Run();err!=nil{return err}}
	}
	return nil
}

func lookupGroup(name string)int{value,err:=user.LookupGroup(name);if err!=nil{return 0};id,err:=strconv.Atoi(value.Gid);if err!=nil{return 0};return id}
func lookupUser(name string)(int,int){value,err:=user.Lookup(name);if err!=nil{return 0,0};uid,_:=strconv.Atoi(value.Uid);gid,_:=strconv.Atoi(value.Gid);return uid,gid}
