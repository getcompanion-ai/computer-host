package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AgentComputerAI/computer-host/internal/model"
	contracthost "github.com/AgentComputerAI/computer-host/contract"
)

type restoredSnapshotArtifact struct {
	Artifact  contracthost.SnapshotArtifact
	LocalPath string
}

func buildSnapshotArtifacts(diskPaths []string) ([]model.SnapshotArtifactRecord, error) {
	artifacts := make([]model.SnapshotArtifactRecord, 0, len(diskPaths))
	for _, diskPath := range diskPaths {
		base := filepath.Base(diskPath)
		diskArtifact, err := snapshotArtifactRecord("disk-"+strings.TrimSuffix(base, filepath.Ext(base)), contracthost.SnapshotArtifactKindDisk, base, diskPath)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, diskArtifact)
	}

	sort.Slice(artifacts, func(i, j int) bool {
		return artifacts[i].ID < artifacts[j].ID
	})
	return artifacts, nil
}

func snapshotArtifactRecord(id string, kind contracthost.SnapshotArtifactKind, name, path string) (model.SnapshotArtifactRecord, error) {
	size, err := fileSize(path)
	if err != nil {
		return model.SnapshotArtifactRecord{}, err
	}
	sum, err := sha256File(path)
	if err != nil {
		return model.SnapshotArtifactRecord{}, err
	}
	return model.SnapshotArtifactRecord{
		ID:        id,
		Kind:      kind,
		Name:      name,
		LocalPath: path,
		SizeBytes: size,
		SHA256Hex: sum,
	}, nil
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %q for sha256: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash %q: %w", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func uploadSnapshotArtifact(ctx context.Context, localPath string, parts []contracthost.SnapshotUploadPart) ([]contracthost.UploadedSnapshotPart, error) {
	if len(parts) == 0 {
		return nil, fmt.Errorf("upload session has no parts")
	}

	file, err := os.Open(localPath)
	if err != nil {
		return nil, fmt.Errorf("open artifact %q: %w", localPath, err)
	}
	defer func() { _ = file.Close() }()

	client := &http.Client{}
	completed := make([]contracthost.UploadedSnapshotPart, 0, len(parts))
	for _, part := range parts {
		reader := io.NewSectionReader(file, part.OffsetBytes, part.SizeBytes)
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, part.UploadURL, io.NopCloser(reader))
		if err != nil {
			return nil, fmt.Errorf("build upload part %d: %w", part.PartNumber, err)
		}
		req.ContentLength = part.SizeBytes

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("upload part %d: %w", part.PartNumber, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("upload part %d returned %d", part.PartNumber, resp.StatusCode)
		}
		etag := strings.TrimSpace(resp.Header.Get("ETag"))
		if etag == "" {
			return nil, fmt.Errorf("upload part %d returned empty etag", part.PartNumber)
		}
		completed = append(completed, contracthost.UploadedSnapshotPart{
			PartNumber: part.PartNumber,
			ETag:       etag,
		})
	}
	sort.Slice(completed, func(i, j int) bool {
		return completed[i].PartNumber < completed[j].PartNumber
	})
	return completed, nil
}

func downloadDurableSnapshotArtifacts(ctx context.Context, root string, artifacts []contracthost.SnapshotArtifact) (map[string]restoredSnapshotArtifact, error) {
	if len(artifacts) == 0 {
		return nil, fmt.Errorf("restore snapshot is missing artifacts")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create restore staging dir %q: %w", root, err)
	}

	client := &http.Client{}
	restored := make(map[string]restoredSnapshotArtifact, len(artifacts))
	for _, artifact := range artifacts {
		if strings.TrimSpace(artifact.DownloadURL) == "" {
			return nil, fmt.Errorf("artifact %q is missing download url", artifact.ID)
		}
		localPath := filepath.Join(root, artifact.Name)
		if err := downloadSnapshotArtifact(ctx, client, artifact.DownloadURL, localPath); err != nil {
			return nil, err
		}
		if expectedSHA := strings.TrimSpace(artifact.SHA256Hex); expectedSHA != "" {
			actualSHA, err := sha256File(localPath)
			if err != nil {
				return nil, err
			}
			if !strings.EqualFold(actualSHA, expectedSHA) {
				_ = os.Remove(localPath)
				return nil, fmt.Errorf("restore artifact %q sha256 mismatch: got %s want %s", artifact.Name, actualSHA, expectedSHA)
			}
		}
		restored[artifact.Name] = restoredSnapshotArtifact{
			Artifact:  artifact,
			LocalPath: localPath,
		}
	}
	return restored, nil
}

func downloadSnapshotArtifact(ctx context.Context, client *http.Client, sourceURL, targetPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return fmt.Errorf("build restore download request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download durable snapshot artifact: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download durable snapshot artifact returned %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return fmt.Errorf("create restore artifact dir %q: %w", filepath.Dir(targetPath), err)
	}
	out, err := os.Create(targetPath)
	if err != nil {
		return fmt.Errorf("create restore artifact %q: %w", targetPath, err)
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("write restore artifact %q: %w", targetPath, err)
	}
	return nil
}
