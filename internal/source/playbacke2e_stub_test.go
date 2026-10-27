//go:build !playbacke2e

package source

import "testing"

func TestSupportedSourceTypeKeepsPlaybackFixtureOutOfProduction(t *testing.T) {
	for _, sourceType := range []string{"local", "webdav", "openlist", "bilibili"} {
		if !SupportedSourceType(sourceType) {
			t.Errorf("production source type %q is not supported", sourceType)
		}
	}
	if SupportedSourceType(playbackE2EFixtureSourceTypeLiteral) {
		t.Fatal("playback E2E source type leaked into a production build")
	}
}

const playbackE2EFixtureSourceTypeLiteral = "playbacke2e-dash"
