package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/decentralabs/lab-station-linux/internal/agent"
	"github.com/decentralabs/lab-station-linux/internal/config"
)

func main() {
	path:=os.Getenv("LABSTATION_CONFIG");if path==""{path="/etc/decentralabs/lab-station/station.toml"}
	cfg,err:=config.Load(path);if err!=nil{fmt.Fprintln(os.Stderr,"labstationd configuration is invalid");os.Exit(2)}
	ctx,cancel:=signal.NotifyContext(context.Background(),syscall.SIGINT,syscall.SIGTERM);defer cancel()
	if err:=agent.New(cfg).Run(ctx);err!=nil{fmt.Fprintln(os.Stderr,"labstationd stopped with an error");os.Exit(2)}
}
