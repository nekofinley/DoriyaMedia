package api

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// proxy_range.go: the progressive proxy exists so a source that cannot be
// redirected to (WebDAV, some cloud drives) still plays without transcoding —
// AVPlayer and hls.js then drive their own buffering and seeking. That only works
// if range requests behave, and upstreams are not uniformly well behaved:
//
//   - some ignore Range entirely and answer 200 with the whole file, which turns
//     every seek into a full download and can stall playback;
//   - some answer 206 with a Content-Range that does not parse.
//
// So the proxy does not merely pass bytes through. It checks what came back and,
// when the upstream ignored the range, answers the range itself by skipping the
// prefix and cutting the tail. That costs a wasted prefix download, but only on
// servers that ignore Range; a server that honours it is passed through untouched.

// byteRange is a parsed Range request for a single range.
type byteRange struct {
	start int64
	end   int64 // -1 means "to the end"
}

// parseRequestRange parses a single-range request header ("bytes=100-199",
// "bytes=100-"). Multi-range requests are rejected: they are vanishingly rare
// from media players and would need multipart handling to answer correctly.
func parseRequestRange(v string) (byteRange, bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(strings.ToLower(v), "bytes=") {
		return byteRange{}, false
	}
	spec := strings.TrimSpace(v[len("bytes="):])
	if strings.Contains(spec, ",") {
		return byteRange{}, false
	}
	dash := strings.Index(spec, "-")
	if dash < 0 {
		return byteRange{}, false
	}
	startTxt := strings.TrimSpace(spec[:dash])
	endTxt := strings.TrimSpace(spec[dash+1:])
	if startTxt == "" {
		return byteRange{}, false // suffix ranges ("-500") are not worth handling
	}
	start, err := strconv.ParseInt(startTxt, 10, 64)
	if err != nil || start < 0 {
		return byteRange{}, false
	}
	if endTxt == "" {
		return byteRange{start: start, end: -1}, true
	}
	end, err := strconv.ParseInt(endTxt, 10, 64)
	if err != nil || end < start {
		return byteRange{}, false
	}
	return byteRange{start: start, end: end}, true
}

// parseContentRange parses a "bytes 0-99/4122738" response header. A total of
// "*" is reported as -1.
func parseContentRange(v string) (start, end, total int64, ok bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(strings.ToLower(v), "bytes ") {
		return 0, 0, 0, false
	}
	spec := strings.TrimSpace(v[len("bytes "):])
	slash := strings.LastIndex(spec, "/")
	if slash < 0 {
		return 0, 0, 0, false
	}
	spanTxt, totalTxt := spec[:slash], strings.TrimSpace(spec[slash+1:])
	dash := strings.Index(spanTxt, "-")
	if dash < 0 {
		return 0, 0, 0, false
	}
	start, err1 := strconv.ParseInt(strings.TrimSpace(spanTxt[:dash]), 10, 64)
	end, err2 := strconv.ParseInt(strings.TrimSpace(spanTxt[dash+1:]), 10, 64)
	if err1 != nil || err2 != nil || start > end {
		return 0, 0, 0, false
	}
	total = -1
	if totalTxt != "*" {
		t, err := strconv.ParseInt(totalTxt, 10, 64)
		if err != nil {
			return 0, 0, 0, false
		}
		total = t
	}
	return start, end, total, true
}

// capsHas reports whether a client capability list ("hevc,av1") contains a name.
func capsHas(caps, name string) bool {
	for _, c := range strings.Split(caps, ",") {
		if strings.EqualFold(strings.TrimSpace(c), name) {
			return true
		}
	}
	return false
}

// audioReports are the audio codecs a client is allowed to vouch for, named the
// way ffprobe names them. The tables in media/decision.go cover what every
// browser of that family plays; these are the ones that depend on the machine
// rather than the browser — AC-3/E-AC-3/DTS decoders live in the OS, so Windows
// and Android usually have them and Linux usually does not.
var audioReports = []string{"ac3", "eac3", "dts", "truehd", "flac", "opus", "vorbis", "alac", "aac", "mp3"}

// reportedAudio reads those tokens out of a capability list ("hevc,eac3"). An
// empty or irrelevant list returns nil — "the client said nothing" — which leaves
// every table verdict exactly as it was.
func reportedAudio(caps string) map[string]bool {
	if strings.TrimSpace(caps) == "" {
		return nil
	}
	var out map[string]bool
	for _, name := range audioReports {
		if capsHas(caps, name) {
			if out == nil {
				out = map[string]bool{}
			}
			out[name] = true
		}
	}
	return out
}

// proxyRemote streams an upstream response, keeping range semantics honest.
func proxyRemote(w http.ResponseWriter, r *http.Request, target string, headers map[string]string) {
	proxyRemoteVia(w, r, target, headers, proxyClient)
}

// proxyRemoteVia is proxyRemote with an explicit client, so a source that
// reaches the network through its own proxy (bilibili from a server abroad) has
// its bytes fetched the same way its API calls are.
func proxyRemoteVia(w http.ResponseWriter, r *http.Request, target string, headers map[string]string, client *http.Client) {
	if client == nil {
		client = proxyClient
	}
	ctx := r.Context()
	req, err := http.NewRequestWithContext(ctx, r.Method, target, nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	requested, wantRange := parseRequestRange(r.Header.Get("Range"))
	if !wantRange {
		if rng := r.Header.Get("Range"); rng != "" {
			req.Header.Set("Range", rng) // unusual shape: let the upstream decide
		}
	}
	if wantRange {
		req.Header.Set("Range", r.Header.Get("Range"))
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "DoriyaMedia/0.1")
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			fail(w, 502, "源请求失败: "+err.Error())
		}
		return
	}
	defer resp.Body.Close()

	// The upstream ignored the range and is sending the whole file as 200.
	if wantRange && resp.StatusCode == http.StatusOK {
		if serveSlicedRange(w, resp, requested) {
			return
		}
		log.Printf("proxy: %s ignored Range %q and sent no usable length; passing it through",
			target, r.Header.Get("Range"))
	}

	// A 206 whose Content-Range does not parse is not something to hand a player:
	// it would take the bytes for a range nobody declared.
	if resp.StatusCode == http.StatusPartialContent {
		if _, _, _, ok := parseContentRange(resp.Header.Get("Content-Range")); !ok {
			log.Printf("proxy: %s answered 206 with an unusable Content-Range %q",
				target, resp.Header.Get("Content-Range"))
			fail(w, 502, "源返回了无法解析的分段响应")
			return
		}
	}

	// HEAD that the upstream rejects: learn what we can with a one-byte GET so the
	// player still sees a length and range support.
	if r.Method == http.MethodHead && resp.StatusCode >= 400 {
		if size, ranges, ok := probeSizeByRange(ctx, target, headers, client); ok {
			w.Header().Set("Accept-Ranges", ranges)
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// serveSlicedRange answers a range request from a full-file 200 response by
// skipping the prefix and cutting the tail. It reports whether it could.
func serveSlicedRange(w http.ResponseWriter, resp *http.Response, want byteRange) bool {
	total := int64(-1)
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			total = n
		}
	}
	if total < 0 || want.start >= total {
		return false
	}
	end := want.end
	if end < 0 || end >= total {
		end = total - 1
	}
	// Discard the prefix the client did not ask for. This is the cost of a source
	// that cannot do ranges itself.
	if want.start > 0 {
		if _, err := io.CopyN(io.Discard, resp.Body, want.start); err != nil {
			return false
		}
	}
	length := end - want.start + 1
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", want.start, end, total))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusPartialContent)
	_, _ = io.CopyN(w, resp.Body, length)
	return true
}

// probeSizeByRange asks for the first byte and reads the total out of the reply,
// for upstreams that refuse HEAD.
func probeSizeByRange(ctx context.Context, target string, headers map[string]string, client *http.Client) (int64, string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, "", false
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Range", "bytes=0-0")
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "DoriyaMedia/0.1")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	ranges := resp.Header.Get("Accept-Ranges")
	if resp.StatusCode == http.StatusPartialContent {
		if _, _, total, ok := parseContentRange(resp.Header.Get("Content-Range")); ok && total >= 0 {
			if ranges == "" {
				ranges = "bytes"
			}
			return total, ranges, true
		}
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			return n, ranges, true
		}
	}
	return 0, ranges, false
}
