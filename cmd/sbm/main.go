package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/d1ys3nk0/sbm/internal/controller"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Printf("sbm %s (%s)\n", version, commit)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	received := make(chan os.Signal, 1)
	go func() {
		select {
		case sig := <-signals:
			received <- sig
			cancel()
		case <-ctx.Done():
		}
	}()
	err := controller.MainContext(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	select {
	case sig := <-received:
		if sig == os.Interrupt {
			os.Exit(130)
		}
		os.Exit(143)
	default:
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, controller.ErrUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
