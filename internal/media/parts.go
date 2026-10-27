package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"doriyamedia/internal/models"
	"doriyamedia/internal/netproxy"
	"doriyamedia/internal/source"
)

// parts.go: files whose media is several upstream streams.
//
// A DASH source (bilibili) hands out the picture and the sound as two URLs that
// each need their own headers. Nothing downstream of the decision expects that:
// the prober reads one URL, the segment builder opens one input, and a client
// pointed at either URL gets half a programme. So such a source declares the
// parts itself (source.MultiPart), the probe is built from that declaration
// instead of from ffprobe, and the segment builder opens one input per part.

// partsEntry is one resolved set of parts, kept for partsTTL.
type partsEntry struct {
	parts []source.Part
	at    time.Time
}

// partsTTL is how long one resolution of a file's parts is reused. It matches
// the single-target cache: re-resolving is one cheap API call, and the links it
// returns outlive it by a wide margin.
const partsTTL = resolveTTL

// partsForKey identifies one resolution of a file's streams. The quality is part
// of it: a bilibili source resolves a different rung (and different URLs) for
// each selection, and handing a cached 1080P parts set to a request that asked
// for 360P would make the menu a no-op — the picture would never change.
func partsForKey(file *models.MediaFile, drv source.Driver) string {
	return fmt.Sprintf("%s|q%d", resolveKey(file), source.QualityOf(drv))
}

// PartsFor returns the upstream streams of a file, or nil when the file is an
// ordinary single-target one.
//
// Resolution is cached per file and quality, and serialised per file, mirroring
// resolveTarget. `rejected` is the link the caller just saw the source refuse:
// the cached set is then dropped and a fresh one resolved, unless somebody
// already did that while the caller waited for the lock.
func (e *Engine) PartsFor(ctx context.Context, file *models.MediaFile, drv source.Driver, rejected string) ([]source.Part, error) {
	mp, ok := drv.(source.MultiPart)
	if !ok {
		return nil, nil
	}
	key := partsForKey(file, drv)
	bypassCache := false
	if bypass, ok := drv.(source.BypassPartsCache); ok {
		bypassCache = bypass.BypassPartsCache()
	}
	mu := e.resolveLock(key)
	handedOff := false
	defer func() {
		if !handedOff {
			e.releaseResolveLock(mu)
		}
	}()
	if err := mu.LockContext(ctx); err != nil {
		return nil, err
	}
	releaseLock := true
	defer func() {
		if releaseLock {
			mu.Unlock()
		}
	}()

	if !bypassCache {
		e.partsMu.Lock()
		if ent, ok := e.partsCache[key]; ok {
			switch {
			case rejected == "" && time.Since(ent.at) < partsTTL:
				e.partsMu.Unlock()
				return ent.parts, nil
			case rejected != "" && leadURL(ent.parts) != rejected:
				e.partsMu.Unlock()
				return ent.parts, nil
			}
			delete(e.partsCache, key)
		}
		e.partsMu.Unlock()
	}

	type partsResult struct {
		parts []source.Part
		err   error
	}
	resultCh := make(chan partsResult, 1)
	go func() {
		parts, err := mp.Parts(ctx, file.Path)
		resultCh <- partsResult{parts: parts, err: err}
	}()
	var result partsResult
	select {
	case result = <-resultCh:
	case <-ctx.Done():
		// A driver that ignores cancellation still owns the per-file lock. Hand
		// the unlock and reference to its watcher so a retry cannot use the driver
		// concurrently and the registry cannot prune the live lock entry.
		releaseLock = false
		handedOff = true
		go func() {
			<-resultCh
			mu.Unlock()
			e.releaseResolveLock(mu)
		}()
		return nil, ctx.Err()
	}
	if result.err != nil {
		return nil, result.err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parts := orderParts(result.parts)
	if len(parts) == 0 {
		return nil, fmt.Errorf("源没有给出可播放的流")
	}
	if !bypassCache {
		e.partsMu.Lock()
		if len(e.partsCache) > 512 {
			e.partsCache = map[string]partsEntry{}
		}
		e.partsCache[key] = partsEntry{parts: parts, at: time.Now()}
		e.partsMu.Unlock()
	}
	return parts, nil
}

// orderParts puts the picture first and drops entries without a URL, so the
// segment builder can map input 0 to video and input 1 to audio without having
// to know which source it is talking to.
func orderParts(parts []source.Part) []source.Part {
	out := make([]source.Part, 0, len(parts))
	for _, p := range parts {
		if p.URL != "" && p.Kind != "audio" {
			out = append(out, p)
		}
	}
	for _, p := range parts {
		if p.URL != "" && p.Kind == "audio" {
			out = append(out, p)
		}
	}
	return out
}

func leadURL(parts []source.Part) string {
	if len(parts) == 0 {
		return ""
	}
	return parts[0].URL
}

// partsProbe builds the probe for a multi-part file out of what the source said
// about it. Probing with ffprobe is not an option — one pass reads one URL, and
// the audio lives at another — but nothing is lost: the codecs, the picture size
// and the duration all come from the source's own API, which is the same
// authority ffprobe would have been reading.
func partsProbe(parts []source.Part) *Probe {
	p := &Probe{Format: ProbeFormat{FormatName: "mov,mp4,m4a,3gp,3g2,mj2"}}
	for i, part := range parts {
		st := ProbeStream{
			Index:          i,
			CodecName:      part.Codec,
			CodecTagString: part.Tag,
			Profile:        part.Profile,
			BitDepth:       part.BitDepth,
			HDR:            part.HDR,
			Width:          part.Width,
			Height:         part.Height,
			Channels:       part.Channels,
		}
		if part.Kind == "audio" {
			st.CodecType = "audio"
		} else {
			st.CodecType = "video"
		}
		if part.Duration > 0 && p.Format.Duration == "" {
			p.Format.Duration = json.Number(strconv.FormatFloat(part.Duration, 'f', 3, 64))
		}
		p.Streams = append(p.Streams, st)
	}
	return p
}

// ProbeFor returns the probe to decide with, or nil when the ordinary ffprobe
// path has to run.
//
// A multi-part source states its own streams, so its probe is built here and
// never read from the files table: what ffprobe would see on the single video
// URL is a video-only stream, and deciding on that is exactly what produces a
// silent picture.
func (e *Engine) ProbeFor(ctx context.Context, file *models.MediaFile, drv source.Driver) (*Probe, error) {
	if file == nil || drv == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := drv.(source.MultiPart); ok {
		parts, err := e.PartsFor(ctx, file, drv, "")
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			return nil, fmt.Errorf("源没有给出可播放的流")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return partsProbe(parts), nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return ProbeFromCache(file), nil
}

// Input is one ffmpeg input: a URL (or filesystem path) plus the request headers
// it needs and, when the source sits behind one, the proxy to reach it through.
// An ordinary file has one; a source that publishes the picture and the sound
// separately has two, in that order.
type Input struct {
	Target  string
	Headers map[string]string
	// Proxy is an http:// URL ffmpeg tunnels through (see ProxyArgs). It is set
	// for a source that configures its own proxy, and it is a loopback bridge
	// rather than the proxy itself, because ffmpeg cannot dial SOCKS5 or
	// Shadowsocks.
	Proxy string
}

// SegmentInputs resolves what a playback has to read: every part for a source
// that publishes several, otherwise the single target. `rejected` forces a fresh
// resolution of the link the caller just saw refused.
func (e *Engine) SegmentInputs(ctx context.Context, file *models.MediaFile, drv source.Driver, rejected string) ([]Input, error) {
	proxy, err := sourceProxyFor(drv)
	if err != nil {
		return nil, err
	}
	if _, ok := drv.(source.MultiPart); ok {
		parts, err := e.PartsFor(ctx, file, drv, rejected)
		if err != nil {
			return nil, err
		}
		ins := make([]Input, 0, len(parts))
		for _, p := range parts {
			ins = append(ins, Input{Target: p.URL, Headers: p.Headers, Proxy: proxy})
		}
		if len(ins) > 0 {
			return ins, nil
		}
	}
	target, headers, err := e.resolveTarget(ctx, file, drv, rejected)
	if err != nil {
		return nil, err
	}
	return []Input{{Target: target, Headers: headers, Proxy: proxy}}, nil
}

// sameInputs reports whether two segment input sets read the same URLs, which is
// what tells a retry that the source link was refreshed rather than reused.
func sameInputs(a, b []Input) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Target != b[i].Target {
			return false
		}
	}
	return true
}

// sourceProxyFor answers the loopback bridge URL a driver's ffmpeg inputs must
// tunnel through, or "" when the driver has no proxy of its own.
//
// Only a source that configures its own proxy has one: the scraper proxies are
// deliberately not inherited, because they are configured for TMDB / Bangumi and
// a source that needs a different route says so itself. A configured bridge that
// cannot be created fails closed; silently direct-dialing would bypass the
// administrator's chosen egress and may expose the server's real address.
func sourceProxyFor(drv source.Driver) (string, error) {
	pd, ok := drv.(source.ProxiedDriver)
	if !ok || strings.TrimSpace(pd.ProxySpec()) == "" {
		return "", nil
	}
	spec, err := netproxy.Parse(pd.ProxySpec())
	if err != nil {
		return "", errors.New("source proxy configuration invalid")
	}
	b, err := netproxy.BridgeFor(spec)
	if err != nil {
		return "", errors.New("source proxy bridge unavailable")
	}
	return b.URL(), nil
}
