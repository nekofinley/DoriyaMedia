//go:build windows

package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestLocalFileVersionWindowsUsesFileIdentityAndChangeTime(t *testing.T) {
	if got := unsafe.Sizeof(localWindowsFileBasicInfo{}); got != 40 {
		t.Fatalf("FILE_BASIC_INFO size = %d, want 40", got)
	}

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

	before := readLocalFileVersionWindows(t, target)
	if before.volume == 0 && before.fileID == 0 {
		t.Fatal("file identity is empty")
	}
	if before.changeTime == 0 {
		t.Fatal("change time is empty")
	}
	var identity windows.ByHandleFileInformation
	f := openLocalFileVersionFixture(t, target)
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &identity); err != nil {
		_ = f.Close()
		t.Fatalf("GetFileInformationByHandle: %v", err)
	}
	_ = f.Close()
	wantID := uint64(identity.FileIndexHigh)<<32 | uint64(identity.FileIndexLow)
	if before.volume != uint64(identity.VolumeSerialNumber) || before.fileID != wantID {
		t.Fatalf("file identity = %x:%x, want %x:%x",
			before.volume, before.fileID,
			identity.VolumeSerialNumber, wantID)
	}

	time.Sleep(25 * time.Millisecond)
	if err := os.WriteFile(target, []byte(replacement), 0o600); err != nil {
		t.Fatalf("rewrite media fixture: %v", err)
	}
	if err := os.Chtimes(target, modTime, modTime); err != nil {
		t.Fatalf("restore media fixture time: %v", err)
	}
	after := readLocalFileVersionWindows(t, target)

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

func readLocalFileVersionWindows(t *testing.T, path string) localFileVersion {
	t.Helper()
	f := openLocalFileVersionFixture(t, path)
	defer f.Close()
	version, ok, err := localFileVersionForFile(f)
	if err != nil {
		t.Fatalf("localFileVersionForFile: %v", err)
	}
	if !ok {
		t.Fatal("local file version is unsupported for a regular NTFS fixture")
	}
	return version
}

func openLocalFileVersionFixture(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open local file version fixture: %v", err)
	}
	return f
}
