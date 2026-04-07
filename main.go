package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	appconfig "github.com/getcompanion-ai/computer-host/internal/config"
	"github.com/getcompanion-ai/computer-host/internal/firecracker"
	"github.com/getcompanion-ai/computer-host/internal/service"
)

type options struct {
	MachineID firecracker.MachineID
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts, err := parseOptions(os.Args[1:], os.Stderr)
	if err != nil {
		exit(err)
	}

	cfg, err := appconfig.Load()
	if err != nil {
		exit(err)
	}

	svc, err := service.New(cfg)
	if err != nil {
		exit(err)
	}

	state, err := svc.CreateMachine(ctx, service.CreateMachineRequest{ID: opts.MachineID})
	if err != nil {
		exit(err)
	}

	if err := writeJSON(os.Stdout, state); err != nil {
		exit(err)
	}
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("firecracker-host", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var machineID string
	fs.StringVar(&machineID, "machine-id", "", "machine id to boot")

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}

	machineID = strings.TrimSpace(machineID)
	if machineID == "" {
		return options{}, fmt.Errorf("-machine-id is required")
	}

	return options{MachineID: firecracker.MachineID(machineID)}, nil
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func exit(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
