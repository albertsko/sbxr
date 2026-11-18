//go:build linux

// Command sbxr-agent is the guest agent of a pod VM. libkrun's init starts it as PID 1.
package main

import (
	"os"

	"github.com/albertsko/sbxr/internal/kube/agent"
)

func main() {
	os.Exit(agent.Main())
}
