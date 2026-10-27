//go:build !windows && !linux

package api

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalFileVersionUnsupportedPlatform(t *testing.T) {
	target := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(target, []byte("0123456789"), 0o600); err != nil {
		t.Fatalf("write media fixture: %v", err)
	}
	f, err := os.Open(target)
	if err != nil {
		t.Fatalf("open media fixture: %v", err)
	}
	defer f.Close()

	version, ok, err := localFileVersionForFile(f)
	if err != nil {
		t.Fatalf("localFileVersionForFile: %v", err)
	}
	if ok {
		t.Fatal("unsupported platform unexpectedly reported a reliable file version")
	}
	if got := v1LocalMediaETag(10, version, ok); got != "" {
		t.Fatalf("unsupported platform ETag = %q, want empty", got)
	}
}
