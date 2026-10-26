package media

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
)

// safeMediaLogError reduces an error to a non-sensitive class. Media errors
// often wrap ffmpeg/ffprobe stderr, which can contain complete source URLs,
// query credentials, local paths, and response headers. Those details must not
// cross into server logs.
func safeMediaLogError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return fmt.Sprintf("process exit status %d", code)
		}
		return "process terminated"
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return fmt.Sprintf("filesystem %s failed", pathErr.Op)
	}
	return "operation failed"
}

// SafeMediaLogError is the exported boundary for callers outside the media
// package that log a probe, ffmpeg, or source-resolution failure. Only a fixed
// error class escapes; wrapped stderr, URLs, query strings, paths, and response
// headers remain inside the media package.
func SafeMediaLogError(err error) string {
	return safeMediaLogError(err)
}

// safeProbeStatusError is the user-facing projection used by the legacy probe
// status endpoint. The no-video verdict is intentional UI text; all other
// failures are reduced to a generic message so source details cannot leak.
func safeProbeStatusError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errNoVideoStream) {
		return errNoVideoStream.Error()
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	default:
		return "media probe failed"
	}
}
