package config

import "testing"

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
