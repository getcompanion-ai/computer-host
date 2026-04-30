package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	appconfig "github.com/AgentComputerAI/computer-host/internal/config"
	"github.com/AgentComputerAI/computer-host/internal/daemon"
	"github.com/AgentComputerAI/computer-host/internal/firecracker"
	"github.com/AgentComputerAI/computer-host/internal/httpapi"
	"github.com/AgentComputerAI/computer-host/internal/store"
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

	unixListener, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		exit(err)
	}
	defer func() {
		_ = unixListener.Close()
	}()

	servers := []*http.Server{{Handler: handler.Routes()}}
	listeners := []net.Listener{unixListener}
	if cfg.HTTPAddr != "" {
		httpListener, err := net.Listen("tcp", cfg.HTTPAddr)
		if err != nil {
			exit(err)
		}
		defer func() {
			_ = httpListener.Close()
		}()
		servers = append(servers, &http.Server{Handler: handler.Routes()})
		listeners = append(listeners, httpListener)
	}

	group, groupCtx := errgroup.WithContext(ctx)
	for i := range servers {
		server := servers[i]
		listener := listeners[i]
		group.Go(func() error {
			err := server.Serve(listener)
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		})
	}
	group.Go(func() error {
		<-groupCtx.Done()
		for _, server := range servers {
			_ = server.Shutdown(context.Background())
		}
		return nil
	})
	group.Go(func() error {
		ticker := time.NewTicker(cfg.ReconcileInterval)
		defer ticker.Stop()

		for {
			select {
			case <-groupCtx.Done():
				return nil
			case <-ticker.C:
				if err := hostDaemon.Reconcile(groupCtx); err != nil && groupCtx.Err() == nil {
					fmt.Fprintf(os.Stderr, "warning: firecracker-host reconcile failed: %v\n", err)
				}
			}
		}
	})

	if err := group.Wait(); err != nil {
		exit(err)
	}
}

func exit(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
