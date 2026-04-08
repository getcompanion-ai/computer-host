package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	appconfig "github.com/getcompanion-ai/computer-host/internal/config"
	"github.com/getcompanion-ai/computer-host/internal/daemon"
	"github.com/getcompanion-ai/computer-host/internal/firecracker"
	"github.com/getcompanion-ai/computer-host/internal/httpapi"
	"github.com/getcompanion-ai/computer-host/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := appconfig.Load()
	if err != nil {
		exit(err)
	}

	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		exit(err)
	}

	runtime, err := firecracker.NewRuntime(cfg.FirecrackerRuntimeConfig())
	if err != nil {
		exit(err)
	}

	hostDaemon, err := daemon.New(cfg, fileStore, runtime)
	if err != nil {
		exit(err)
	}
	if err := hostDaemon.Reconcile(ctx); err != nil {
		exit(err)
	}

	handler, err := httpapi.New(hostDaemon)
	if err != nil {
		exit(err)
	}

	if err := os.MkdirAll(filepath.Dir(cfg.SocketPath), 0o755); err != nil {
		exit(err)
	}
	if err := os.Remove(cfg.SocketPath); err != nil && !os.IsNotExist(err) {
		exit(err)
	}

	listener, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		exit(err)
	}
	defer listener.Close()

	server := &http.Server{Handler: handler.Routes()}
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.Background())
	}()

	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		exit(err)
	}
}

func exit(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
