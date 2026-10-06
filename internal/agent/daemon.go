package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (a *Agent) Run(ctx context.Context) error {
	if err:=os.MkdirAll(a.Config.StateDir,0750);err!=nil{return err}
	if err:=os.MkdirAll(filepath.Join(a.Config.StateDir,"commands","inbox"),0750);err!=nil{return err}
	if err:=os.MkdirAll(filepath.Join(a.Config.StateDir,"commands","processing"),0750);err!=nil{return err}
	if err:=os.MkdirAll(filepath.Join(a.Config.StateDir,"commands","processed"),0750);err!=nil{return err}
	if err:=os.MkdirAll(filepath.Join(a.Config.StateDir,"commands","results"),0750);err!=nil{return err}
	if err:=os.MkdirAll(a.Config.LogDir,0750);err!=nil{return err}
	statusTimer:=time.NewTicker(15*time.Second);defer statusTimer.Stop()
	queueTimer:=time.NewTicker(750*time.Millisecond);defer queueTimer.Stop()
	if err:=a.publish();err!=nil{return err}
	for {
		select {
		case <-ctx.Done(): return nil
		case <-statusTimer.C: if err:=a.publish();err!=nil{a.log("heartbeat-write-failed",map[string]any{"error":err.Error()})}
		case <-queueTimer.C: if err:=a.processQueue(ctx);err!=nil{a.log("queue-error",map[string]any{"error":err.Error()})}
		}
	}
}

func (a *Agent) publish()error {
	status:=a.Status()
	if err:=atomicJSON(filepath.Join(a.Config.StateDir,"status.json"),status);err!=nil{return err}
	if err:=atomicJSON(filepath.Join(a.Config.StateDir,"heartbeat.json"),status);err!=nil{return err}
	a.log("heartbeat-published",map[string]any{"profile":a.Config.Profile,"ready":status["summary"].(map[string]any)["ready"]})
	return nil
}

func (a *Agent) processQueue(ctx context.Context)error {
	inbox:=filepath.Join(a.Config.StateDir,"commands","inbox")
	entries,err:=os.ReadDir(inbox);if err!=nil{return err}
	for _,entry:=range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(),".json") {continue}
		id:=strings.TrimSuffix(entry.Name(),".json")
		if len(id)>96 || strings.ContainsAny(id,"/\\\r\n") {continue}
		claim:=filepath.Join(a.Config.StateDir,"commands","processing",entry.Name())
		if err:=os.Rename(filepath.Join(inbox,entry.Name()),claim);err!=nil{if errors.Is(err,fs.ErrNotExist){continue};return err}
		result:=a.processQueueRequest(ctx,claim,id)
		resultPath:=filepath.Join(a.Config.StateDir,"commands","results",id+".json")
		if err:=atomicJSON(resultPath,result);err!=nil{return err}
		_ = os.Rename(claim,filepath.Join(a.Config.StateDir,"commands","processed",entry.Name()))
		a.log("queue-command-complete",map[string]any{"operationId":id,"command":result.Command,"exitCode":result.ExitCode,"outcome":result.Outcome})
	}
	return nil
}

func (a *Agent) processQueueRequest(ctx context.Context,path,id string)Result {
	data,err:=os.ReadFile(path);if err!=nil{return a.rejected(id,"unable to read queued command")}
	var request Request
	decoder:=json.NewDecoder(strings.NewReader(string(data)));decoder.DisallowUnknownFields()
	if err=decoder.Decode(&request);err!=nil{return a.rejected(id,"queued command is invalid")}
	if request.SchemaVersion!=1 || request.Operation!="execute" || request.ID!=id{return a.rejected(id,"queued command identity is invalid")}
	if request.SecretValue!="" || request.SecretID!=""{return a.rejected(id,"secrets are not accepted through the queue")}
	return a.Execute(ctx,request.ID,request.Command,request.Args)
}

func (a *Agent) rejected(id,message string)Result{return Result{ID:id,Command:"queue",CompletedAt:time.Now().UTC().Format(time.RFC3339Nano),Success:false,ExitCode:2,Outcome:"failure",Message:message,Stderr:message,Metadata:map[string]any{"code":"STATION_COMMAND_REJECTED"}}}

func (a *Agent) log(event string,fields map[string]any) {
	fields["event"]=event
	fields["timestamp"]=time.Now().UTC().Format(time.RFC3339Nano)
	fields["host"]=a.Config.Name
	fields["profile"]=a.Config.Profile
	file,err:=os.OpenFile(filepath.Join(a.Config.LogDir,"labstation.log"),os.O_CREATE|os.O_APPEND|os.O_WRONLY,0640);if err!=nil{return};defer file.Close();_ = json.NewEncoder(file).Encode(fields)
}
