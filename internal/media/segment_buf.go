package media

import (
	"bytes"
	"io"
	"os"
)

// segSpillLimit is how much of one segment is held in memory before the rest goes
// to a temporary file. A four-second 4K segment runs to tens of megabytes, and
// several viewers asking at once must not add up to that in RAM.
const segSpillLimit = 8 << 20

// segBuf collects one segment so it can be handed to the player in one piece.
//
// ffmpeg's output used to be streamed straight into the HTTP response, which meant
// an attempt that died halfway had already delivered a truncated fMP4. A player
// cannot tell that apart from a decoding failure: it reports a media error and
// refuses to retry the segment, so a stream that would have played on the next
// attempt shows a failure card instead. Holding the bytes until ffmpeg exits turns
// that into an honest failure the player retries — and, because nothing was sent,
// the retry-with-a-fresh-source-link path stays available even when the first
// attempt produced some output before dying.
type segBuf struct {
	mem  bytes.Buffer
	file *os.File
}

func (b *segBuf) Write(p []byte) (int, error) {
	if b.file == nil && b.mem.Len()+len(p) > segSpillLimit {
		f, err := os.CreateTemp("", "dm-segment-*")
		if err != nil {
			// No temp file available: keep it all in memory rather than fail a
			// segment that is otherwise fine.
			return b.mem.Write(p)
		}
		if _, err := f.Write(b.mem.Bytes()); err != nil {
			f.Close()
			os.Remove(f.Name())
			return 0, err
		}
		b.mem.Reset()
		b.file = f
	}
	if b.file != nil {
		return b.file.Write(p)
	}
	return b.mem.Write(p)
}

// WriteTo hands everything collected to w.
func (b *segBuf) WriteTo(w io.Writer) (int64, error) {
	if b.file == nil {
		n, err := w.Write(b.mem.Bytes())
		return int64(n), err
	}
	if _, err := b.file.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	return io.Copy(w, b.file)
}

// Reset empties the buffer so another attempt can fill it.
func (b *segBuf) Reset() {
	b.mem.Reset()
	if b.file != nil {
		_ = b.file.Truncate(0)
		_, _ = b.file.Seek(0, io.SeekStart)
	}
}

// Close releases the temporary file, if one was needed.
func (b *segBuf) Close() {
	if b.file != nil {
		name := b.file.Name()
		_ = b.file.Close()
		_ = os.Remove(name)
		b.file = nil
	}
}
