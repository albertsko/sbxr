// Command sbxr-node installs or removes the sbxr runtime on the node it runs on.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/albertsko/sbxr/internal/kube/installer"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	code := installer.Main(ctx, os.Args[1:], slog.New(slog.NewTextHandler(os.Stderr, nil)))

	stop()
	os.Exit(code)
}
