package main

import (
	"fmt"
	"os"

	"github.com/decentralabs/lab-station-linux/internal/agent"
)

func main() {
	if err := agent.ServeDispatcher(); err != nil {
		fmt.Fprintln(os.Stderr, "labstation dispatcher failed")
		os.Exit(2)
	}
}
