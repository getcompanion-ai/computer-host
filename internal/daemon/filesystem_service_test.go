package daemon

import "testing"

func TestResolveWorkspacePathRejectsNewlines(t *testing.T) {
	t.Parallel()

	if _, err := resolveWorkspacePath("dir\nfile.txt"); err == nil {
		t.Fatal("expected newline-containing path to be rejected")
	}
}

func TestApplyReadRangeZeroLengthReturnsEmpty(t *testing.T) {
	t.Parallel()

	chunk, err := applyReadRange([]byte("abcdef"), 2, 0)
	if err != nil {
		t.Fatalf("applyReadRange returned error: %v", err)
	}
	if len(chunk) != 0 {
		t.Fatalf("chunk = %q, want empty", string(chunk))
	}
}

func TestExt4ModeValuesIncludeFileTypeBits(t *testing.T) {
	t.Parallel()

	if got := ext4FileModeValue(0o644); got != "100644" {
		t.Fatalf("ext4FileModeValue = %q, want %q", got, "100644")
	}
	if got := ext4DirModeValue(0o755); got != "040755" {
		t.Fatalf("ext4DirModeValue = %q, want %q", got, "040755")
	}
}

func TestFilterDebugFSOutputPreservesLeadingNewlines(t *testing.T) {
	t.Parallel()

	output := "debugfs 1.47.0\n\nhello\n"
	if got := filterDebugFSOutput(output); got != "\nhello\n" {
		t.Fatalf("filterDebugFSOutput = %q, want %q", got, "\nhello\n")
	}
}

func TestWorkspacePathContains(t *testing.T) {
	t.Parallel()

	if !workspacePathContains("/home/node/workspace/a", "/home/node/workspace/a/b") {
		t.Fatal("expected descendant path to be detected")
	}
	if workspacePathContains("/home/node/workspace/a", "/home/node/workspace/ab") {
		t.Fatal("sibling path should not match as descendant")
	}
}
