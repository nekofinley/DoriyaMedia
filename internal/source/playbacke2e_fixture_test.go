//go:build playbacke2e

package source

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"doriyamedia/internal/models"
)

func TestPlaybackE2EFixtureDriver(t *testing.T) {
	modePath := filepath.Join(t.TempDir(), "mode.txt")
	t.Setenv(playbackE2EModeFile, modePath)
	t.Setenv(playbackE2EVideoURL, "https://video.example.test/video.m4s?token=video")
	t.Setenv(playbackE2EAudioURL, "https://audio.example.test/audio.m4s?token=audio")
	t.Setenv(playbackE2ECookie, "fixture-cookie-secret")
	t.Setenv(playbackE2EAuthorization, "Bearer fixture-authorization-secret")

	driver, err := NewDriver(&models.Source{Type: playbackE2ESourceType})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	if _, ok := driver.(MultiPart); !ok {
		t.Fatal("fixture driver does not implement MultiPart")
	}
	bypass, ok := driver.(BypassPartsCache)
	if !ok || !bypass.BypassPartsCache() {
		t.Fatal("fixture driver does not bypass the parts cache")
	}

	entries, err := driver.List(context.Background(), "/")
	if err != nil || len(entries) != 1 || entries[0].Path != playbackE2EPath {
		t.Fatalf("List = %#v, %v", entries, err)
	}

	if err := os.WriteFile(modePath, []byte("valid\n"), 0o600); err != nil {
		t.Fatalf("write valid mode: %v", err)
	}
	parts, err := driver.(MultiPart).Parts(context.Background(), playbackE2EPath)
	if err != nil {
		t.Fatalf("Parts valid: %v", err)
	}
	if len(parts) != 2 || parts[0].Kind != "video" || parts[1].Kind != "audio" {
		t.Fatalf("valid parts = %#v", parts)
	}
	if _, exists := parts[0].Headers["Cookie"]; exists {
		t.Fatal("valid fixture unexpectedly includes Cookie")
	}

	if err := os.WriteFile(modePath, []byte("bad-header\n"), 0o600); err != nil {
		t.Fatalf("write bad-header mode: %v", err)
	}
	parts, err = driver.(MultiPart).Parts(context.Background(), playbackE2EPath)
	if err != nil {
		t.Fatalf("Parts bad-header: %v", err)
	}
	if parts[0].Headers["Cookie"] == "" || parts[1].Headers["Authorization"] == "" {
		t.Fatalf("bad-header fixture is missing unsafe headers: %#v", parts)
	}

	if err := os.WriteFile(
		modePath,
		[]byte("valid-probe-and-select-then-bad-header\n"),
		0o600,
	); err != nil {
		t.Fatalf("write staged bad-header mode: %v", err)
	}
	stagedDriver, err := NewDriver(&models.Source{Type: playbackE2ESourceType})
	if err != nil {
		t.Fatalf("NewDriver staged: %v", err)
	}
	for call := 1; call <= 3; call++ {
		parts, err = stagedDriver.(MultiPart).Parts(
			context.Background(), playbackE2EPath)
		if err != nil {
			t.Fatalf("staged Parts call %d: %v", call, err)
		}
		if _, exists := parts[0].Headers["Cookie"]; exists {
			t.Fatalf("staged Parts call %d returned unsafe headers before claim", call)
		}
	}
	parts, err = stagedDriver.(MultiPart).Parts(context.Background(), playbackE2EPath)
	if err != nil {
		t.Fatalf("staged Parts claim: %v", err)
	}
	if parts[0].Headers["Cookie"] == "" || parts[1].Headers["Authorization"] == "" {
		t.Fatal("staged claim did not return unsafe headers")
	}
}
