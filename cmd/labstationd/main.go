package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/decentralabs/lab-station-linux/internal/agent"
	"github.com/decentralabs/lab-station-linux/internal/config"
)

func main() {
	path := os.Getenv("LABSTATION_CONFIG")
	if path == "" {
		path = "/etc/decentralabs/lab-station/station.toml"
	}
	cfg, err := config.Load(path)
	if err != nil {
		agent.LogStartupFailure(config.Defaults(), "daemon-configuration-invalid", err)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	station := agent.New(cfg)
	if err := station.Run(ctx); err != nil {
		station.LogProcessFailure("daemon-stopped-with-error", err)
		os.Exit(2)
	}
}
