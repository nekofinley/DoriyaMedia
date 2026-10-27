//go:build linux

package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLocalFileVersionLinuxUsesDeviceInodeAndChangeTime(t *testing.T) {
	const (
		original    = "0123456789"
		replacement = "abcdefghij"
	)
	target := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatalf("write media fixture: %v", err)
	}
	modTime := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(target, modTime, modTime); err != nil {
		t.Fatalf("set media fixture time: %v", err)
	}

	before := readLocalFileVersionLinux(t, target)
	var stat unix.Stat_t
	if err := unix.Stat(target, &stat); err != nil {
		t.Fatalf("stat media fixture: %v", err)
	}
	if before.volume != uint64(stat.Dev) || before.fileID != uint64(stat.Ino) {
		t.Fatalf("file identity = %x:%x, want %x:%x",
			before.volume, before.fileID, stat.Dev, stat.Ino)
	}
	if before.changeTime == 0 {
		t.Fatal("change time is empty")
	}

	time.Sleep(25 * time.Millisecond)
	if err := os.WriteFile(target, []byte(replacement), 0o600); err != nil {
		t.Fatalf("rewrite media fixture: %v", err)
	}
	if err := os.Chtimes(target, modTime, modTime); err != nil {
		t.Fatalf("restore media fixture time: %v", err)
	}
	after := readLocalFileVersionLinux(t, target)

	if after.volume != before.volume || after.fileID != before.fileID {
		t.Fatalf("same-file rewrite changed identity from %x:%x to %x:%x",
			before.volume, before.fileID, after.volume, after.fileID)
	}
	if after.changeTime == before.changeTime {
		t.Fatalf("change time = %x after same-size rewrite with restored mtime",
			after.changeTime)
	}
	if v1LocalMediaETag(int64(len(original)), before, true) ==
		v1LocalMediaETag(int64(len(replacement)), after, true) {
		t.Fatal("ETag did not change after same-size rewrite")
	}
}

func readLocalFileVersionLinux(t *testing.T, path string) localFileVersion {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open local file version fixture: %v", err)
	}
	defer f.Close()
	version, ok, err := localFileVersionForFile(f)
	if err != nil {
		t.Fatalf("localFileVersionForFile: %v", err)
	}
	if !ok {
		t.Fatal("local file version is unsupported for a regular Linux fixture")
	}
	return version
}
