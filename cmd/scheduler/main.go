package main

import (
	"log"
	"os"

	"k8s.io/component-base/logs"
	"k8s.io/kubernetes/cmd/kube-scheduler/app"

	"github.com/ray/gpu-vram-optimizer-k8s/internal/scheduler"
)

func main() {
	logs.InitLogs()
	defer logs.FlushLogs()

	command := app.NewSchedulerCommand(
		app.WithPlugin(scheduler.Name, scheduler.New),
	)

	if err := command.Execute(); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}
