package config

import (
	"testing"

	"github.com/AgentComputerAI/computer-host/internal/firecracker"
)

func TestLoadDiskCloneModeDefaultsToReflink(t *testing.T) {
	t.Parallel()

	if got := loadDiskCloneMode(""); got != DiskCloneModeReflink {
		t.Fatalf("disk clone mode = %q, want %q", got, DiskCloneModeReflink)
	}
}

func TestDiskCloneModeValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mode    DiskCloneMode
		wantErr bool
	}{
		{name: "reflink", mode: DiskCloneModeReflink},
		{name: "copy", mode: DiskCloneModeCopy},
		{name: "empty", mode: "", wantErr: true},
		{name: "unknown", mode: "auto", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.mode.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
		})
	}
}

func TestLoadDriveIOEngineDefaultsToSync(t *testing.T) {
	t.Parallel()

	if got := loadDriveIOEngine(""); got != firecracker.DriveIOEngineSync {
		t.Fatalf("drive io engine = %q, want %q", got, firecracker.DriveIOEngineSync)
	}
}

func TestValidateDriveIOEngine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		engine  firecracker.DriveIOEngine
		wantErr bool
	}{
		{name: "sync", engine: firecracker.DriveIOEngineSync},
		{name: "async", engine: firecracker.DriveIOEngineAsync},
		{name: "empty", engine: "", wantErr: true},
		{name: "unknown", engine: "Aio", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateDriveIOEngine(tt.engine)
			if tt.wantErr && err == nil {
				t.Fatal("validateDriveIOEngine() error = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validateDriveIOEngine() error = %v, want nil", err)
			}
		})
	}
}

func TestLoadBool(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    bool
		wantErr bool
	}{
		{name: "default"},
		{name: "true", value: "true", want: true},
		{name: "false", value: "false"},
		{name: "one", value: "1", want: true},
		{name: "invalid", value: "enabled", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			envName := "TEST_FIRECRACKER_BOOL"
			t.Setenv(envName, tt.value)

			got, err := loadBool(envName)
			if tt.wantErr && err == nil {
				t.Fatal("loadBool() error = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("loadBool() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Fatalf("loadBool() = %v, want %v", got, tt.want)
			}
		})
	}
}
