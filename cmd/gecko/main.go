package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/CM-exe/gecko/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Main(ctx, os.Args[1:], cli.OSEnv())
	stop()
	os.Exit(code)
}
