//go:build !playbacke2e

package source

import "doriyamedia/internal/models"

func newPlaybackE2EDriver(
	_ *models.Source,
	_ models.SourceConfig,
) (Driver, bool, error) {
	return nil, false, nil
}

func playbackE2ESourceTypeSupported(string) bool {
	return false
}
