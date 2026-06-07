package main

import (
	"log"

	"github.com/ray/gpu-vram-optimizer-k8s/internal/telemetry"
)

func main() {
	daemon := telemetry.NewDaemonFromEnv()
	if err := daemon.Run(); err != nil {
		log.Fatalf("telemetry-daemon exited: %v", err)
	}
}
