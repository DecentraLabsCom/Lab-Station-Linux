package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/decentralabs/lab-station-linux/internal/config"
)

var secretPattern = regexp.MustCompile(`^[\x20-\x7e]{32,512}$`)

func Dispatch(input io.Reader, output io.Writer, a *Agent) error {
	decoder:=json.NewDecoder(io.LimitReader(input,64*1024))
	decoder.DisallowUnknownFields()
	var request Request
	if err:=decoder.Decode(&request);err!=nil{return writeRejected(output,"","invalid JSON request")}
	var extra any
	if err:=decoder.Decode(&extra);err!=io.EOF{return writeRejected(output,request.ID,"request must contain one JSON object")}
	if request.SchemaVersion!=1{return writeRejected(output,request.ID,"unsupported dispatcher protocol")}
	ctx,cancel:=context.WithTimeout(context.Background(),5*time.Minute);defer cancel()
	if request.Operation=="execute" {
		result:=a.Execute(ctx,request.ID,request.Command,request.Args)
		return json.NewEncoder(output).Encode(result)
	}
	if request.Operation=="artifact.read" {
		data,err:=a.Artifact(request.Artifact)
		if err!=nil{return encodeResult(output,request.ID,"artifact.read",2,"",err.Error(),map[string]any{"code":"STATION_ARTIFACT_UNAVAILABLE"})}
		return encodeResult(output,request.ID,"artifact.read",0,string(data),"",map[string]any{"artifact":request.Artifact})
	}
	if request.Operation=="secret.set" {
		if request.SecretID!="fmu-internal-token" || !secretPattern.MatchString(request.SecretValue) { return encodeResult(output,request.ID,"secret.set",2,"","secret payload is invalid",map[string]any{"code":"STATION_SECRET_REJECTED"}) }
		value:=request.SecretValue
		err:=a.helper(ctx,map[string]any{"operation":"secret-set","secretId":request.SecretID,"secretValue":value})
		value=""
		if err!=nil{return encodeResult(output,request.ID,"secret.set",2,"","secret provisioning failed",map[string]any{"code":"STATION_SECRET_FAILED"})}
		return encodeResult(output,request.ID,"secret.set",0,"","secret configured",map[string]any{"secretId":request.SecretID})
	}
	if request.Operation=="secret.clear" {
		if request.SecretID!="fmu-internal-token" { return encodeResult(output,request.ID,"secret.clear",2,"","secret identifier is not allowlisted",map[string]any{"code":"STATION_SECRET_REJECTED"}) }
		if err:=a.helper(ctx,map[string]any{"operation":"secret-clear","secretId":request.SecretID});err!=nil{return encodeResult(output,request.ID,"secret.clear",2,"","secret release failed",map[string]any{"code":"STATION_SECRET_FAILED"})}
		return encodeResult(output,request.ID,"secret.clear",0,"","secret cleared",map[string]any{"secretId":request.SecretID})
	}
	return writeRejected(output,request.ID,"dispatcher operation is not allowlisted")
}

func writeRejected(output io.Writer,id,message string)error{return encodeResult(output,id,"dispatcher",2,"",message,map[string]any{"code":"STATION_COMMAND_REJECTED"})}
func encodeResult(output io.Writer,id,command string,code int,stdout,stderr string,metadata map[string]any)error{
	if id==""{id=fmt.Sprintf("local-%d",time.Now().UnixNano())}
	outcome:="success";if code==1{outcome="warning"}else if code>=2{outcome="failure"}
	message:=strings.TrimSpace(stdout);if message==""{message=outcome};if stderr!=""{message=stderr}
	return json.NewEncoder(output).Encode(Result{ID:id,Command:command,CompletedAt:time.Now().UTC().Format(time.RFC3339Nano),Success:code<2,ExitCode:code,Outcome:outcome,Message:message,Stdout:stdout,Stderr:stderr,Metadata:metadata})
}

func ServeDispatcher()error{
	configPath:=os.Getenv("LABSTATION_CONFIG")
	if configPath==""{configPath="/etc/decentralabs/lab-station/station.toml"}
	cfg,err:=loadConfigWithFallback(configPath);if err!=nil{return err}
	if err:=Dispatch(bufio.NewReader(os.Stdin),os.Stdout,New(cfg));err!=nil{return err}
	return nil
}

func loadConfigWithFallback(path string)(config.Config,error){
	cfg,err:=config.Load(path)
	if err==nil{return cfg,nil}
	if errors.Is(err,os.ErrNotExist){cfg=config.Defaults();cfg.Name,_=os.Hostname();return cfg,nil}
	return cfg,err
}
