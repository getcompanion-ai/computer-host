package daemon

import (
	"testing"

	"github.com/getcompanion-ai/computer-host/internal/model"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

func TestBuildRcloneConfigEnvUsesS3ProviderForGCSInteroperability(t *testing.T) {
	t.Parallel()

	env, err := buildRcloneConfigEnv(model.MountRecord{
		Kind: contracthost.MountKindGCS,
		Config: contracthost.MountConfig{
			AccessKeyID: "access",
			Region:      "auto",
		},
	}, "secret")
	if err != nil {
		t.Fatalf("buildRcloneConfigEnv returned error: %v", err)
	}

	if env["RCLONE_CONFIG_REMOTE_TYPE"] != "s3" {
		t.Fatalf("type = %q, want s3", env["RCLONE_CONFIG_REMOTE_TYPE"])
	}
	if env["RCLONE_CONFIG_REMOTE_PROVIDER"] != "GCS" {
		t.Fatalf("provider = %q, want GCS", env["RCLONE_CONFIG_REMOTE_PROVIDER"])
	}
	if env["RCLONE_CONFIG_REMOTE_ENDPOINT"] != "https://storage.googleapis.com" {
		t.Fatalf("endpoint = %q, want default GCS endpoint", env["RCLONE_CONFIG_REMOTE_ENDPOINT"])
	}
	if env["RCLONE_CONFIG_REMOTE_SECRET_ACCESS_KEY"] != "secret" {
		t.Fatal("secret access key was not propagated")
	}
}

func TestBuildRcloneConfigEnvUsesWebDAVSpecificKeys(t *testing.T) {
	t.Parallel()

	env, err := buildRcloneConfigEnv(model.MountRecord{
		Kind: contracthost.MountKindWebDAV,
		Config: contracthost.MountConfig{
			Endpoint:    "https://dav.example.com/files",
			AccessKeyID: "user",
		},
	}, "obscured-pass")
	if err != nil {
		t.Fatalf("buildRcloneConfigEnv returned error: %v", err)
	}

	expected := map[string]string{
		"RCLONE_CONFIG_REMOTE_TYPE":   "webdav",
		"RCLONE_CONFIG_REMOTE_URL":    "https://dav.example.com/files",
		"RCLONE_CONFIG_REMOTE_VENDOR": "other",
		"RCLONE_CONFIG_REMOTE_USER":   "user",
		"RCLONE_CONFIG_REMOTE_PASS":   "obscured-pass",
	}
	for key, want := range expected {
		if got := env[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if _, ok := env["RCLONE_CONFIG_REMOTE_ACCESS_KEY_ID"]; ok {
		t.Fatal("webdav env should not include s3 access key fields")
	}
}

func TestBuildRcloneConfigEnvRequiresWebDAVEndpoint(t *testing.T) {
	t.Parallel()

	if _, err := buildRcloneConfigEnv(model.MountRecord{Kind: contracthost.MountKindWebDAV}, "secret"); err == nil {
		t.Fatal("expected error for missing webdav endpoint")
	}
}

func TestBuildRcloneConfigEnvRequiresR2Endpoint(t *testing.T) {
	t.Parallel()

	if _, err := buildRcloneConfigEnv(model.MountRecord{Kind: contracthost.MountKindR2}, "secret"); err == nil {
		t.Fatal("expected error for missing r2 endpoint")
	}
}
