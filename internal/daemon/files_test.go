package daemon

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	appconfig "github.com/AgentComputerAI/computer-host/internal/config"
)

func TestCloneDiskFileCopyPreservesSparseDiskUsage(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.img")
	targetPath := filepath.Join(root, "target.img")

	sourceFile, err := os.OpenFile(sourcePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open source file: %v", err)
	}
	if _, err := sourceFile.Write([]byte("head")); err != nil {
		_ = sourceFile.Close()
		t.Fatalf("write source prefix: %v", err)
	}
	if _, err := sourceFile.Seek(32<<20, io.SeekStart); err != nil {
		_ = sourceFile.Close()
		t.Fatalf("seek source hole: %v", err)
	}
	if _, err := sourceFile.Write([]byte("tail")); err != nil {
		_ = sourceFile.Close()
		t.Fatalf("write source suffix: %v", err)
	}
	if err := sourceFile.Close(); err != nil {
		t.Fatalf("close source file: %v", err)
	}

	sourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatalf("stat source file: %v", err)
	}
	sourceUsage, err := allocatedBytes(sourcePath)
	if err != nil {
		t.Fatalf("allocated bytes for source: %v", err)
	}
	if sourceUsage >= sourceInfo.Size()/2 {
		t.Skip("temp filesystem does not expose sparse allocation savings")
	}

	if err := cloneDiskFile(sourcePath, targetPath, appconfig.DiskCloneModeCopy); err != nil {
		t.Fatalf("clone sparse file: %v", err)
	}

	targetInfo, err := os.Stat(targetPath)
	if err != nil {
		t.Fatalf("stat target file: %v", err)
	}
	if targetInfo.Size() != sourceInfo.Size() {
		t.Fatalf("target size mismatch: got %d want %d", targetInfo.Size(), sourceInfo.Size())
	}

	targetUsage, err := allocatedBytes(targetPath)
	if err != nil {
		t.Fatalf("allocated bytes for target: %v", err)
	}
	if targetUsage >= targetInfo.Size()/2 {
		t.Fatalf("target file is not sparse enough: allocated=%d size=%d", targetUsage, targetInfo.Size())
	}

	targetData, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read target file: %v", err)
	}
	if !bytes.Equal(targetData[:4], []byte("head")) {
		t.Fatalf("target prefix mismatch: %q", string(targetData[:4]))
	}
	if !bytes.Equal(targetData[len(targetData)-4:], []byte("tail")) {
		t.Fatalf("target suffix mismatch: %q", string(targetData[len(targetData)-4:]))
	}
	if !bytes.Equal(targetData[4:4+(1<<20)], make([]byte, 1<<20)) {
		t.Fatal("target hole contents were not zeroed")
	}
}

func TestCloneDiskFileReflinkMode(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.img")
	targetPath := filepath.Join(root, "target.img")

	if err := os.WriteFile(sourcePath, []byte("rootfs"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}

	err := cloneDiskFile(sourcePath, targetPath, appconfig.DiskCloneModeReflink)
	if err != nil {
		t.Skipf("temp filesystem does not support reflinks: %v", err)
	}

	targetData, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read target file: %v", err)
	}
	if !bytes.Equal(targetData, []byte("rootfs")) {
		t.Fatalf("target data mismatch: %q", string(targetData))
	}
}

func allocatedBytes(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, syscall.EINVAL
	}
	return stat.Blocks * 512, nil
}
