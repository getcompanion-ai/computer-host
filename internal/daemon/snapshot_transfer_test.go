package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

func TestUploadSnapshotArtifactRejectsEmptyETag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("unexpected method %q", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	artifactPath := filepath.Join(t.TempDir(), "artifact.bin")
	if err := os.WriteFile(artifactPath, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	_, err := uploadSnapshotArtifact(context.Background(), artifactPath, []contracthost.SnapshotUploadPart{{
		PartNumber:  1,
		OffsetBytes: 0,
		SizeBytes:   int64(len("payload")),
		UploadURL:   server.URL,
	}})
	if err == nil || !strings.Contains(err.Error(), "empty etag") {
		t.Fatalf("uploadSnapshotArtifact error = %v, want empty etag failure", err)
	}
}

func TestDownloadDurableSnapshotArtifactsRejectsSHA256Mismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer server.Close()

	root := t.TempDir()
	_, err := downloadDurableSnapshotArtifacts(context.Background(), root, []contracthost.SnapshotArtifact{{
		ID:          "memory",
		Kind:        contracthost.SnapshotArtifactKindMemory,
		Name:        "memory.bin",
		DownloadURL: server.URL,
		SHA256Hex:   strings.Repeat("0", 64),
	}})
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("downloadDurableSnapshotArtifacts error = %v, want sha256 mismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "memory.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("corrupt artifact should be removed, stat err = %v", statErr)
	}
}
