package media

import (
	"context"
	"log"

	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

// RetryWithFreshLink runs attempt, and if it fails, resolves the source link again
// and runs it once more with the new one.
//
// A cold file is resolved twice on its first play: once so it can be probed, once
// so it can be packaged. Cloud drives commonly invalidate the previous link when a
// new one is issued — Segment has always retried on exactly that — so a packager
// can be left holding a link the source has just refused. The session path had no
// such retry, and the result was the shape of a bug report: the first play fails,
// the viewer presses retry, and the retry resolves a new link and works.
//
// Only usable for work that has produced no output when it fails: the caller's
// attempt must not have written anything before returning its error.
func RetryWithFreshLink(ctx context.Context, e *Engine, file *models.MediaFile, drv source.Driver,
	inputs []Input, attempt func(inputs []Input) error) error {

	err := attempt(inputs)
	if err == nil || ctx.Err() != nil {
		return err
	}
	rejected := ""
	if len(inputs) > 0 {
		rejected = inputs[0].Target
	}
	fresh, rerr := e.SegmentInputs(ctx, file, drv, rejected)
	if rerr != nil {
		return err
	}
	log.Printf("hls session for file %d failed, retrying with a fresh source link: %v", file.ID, err)
	if err2 := attempt(fresh); err2 != nil {
		return err2
	}
	log.Printf("hls session for file %d recovered with a fresh source link", file.ID)
	return nil
}
