package api

import (
	"context"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
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

// legacyRangeRequest keeps the suffix form that older browser clients use.
// The v1 proxy deliberately accepts only forward ranges, but the legacy web
// player still sends "bytes=-N" and must keep working.
type legacyRangeRequest struct {
	start  int64
	end    int64 // -1 means "to the end"
	suffix int64 // positive for a suffix range
}

func (r legacyRangeRequest) isSuffix() bool {
	return r.suffix > 0
}

func (r legacyRangeRequest) headerValue() string {
	if r.isSuffix() {
		return fmt.Sprintf("bytes=-%d", r.suffix)
	}
	if r.end < 0 {
		return fmt.Sprintf("bytes=%d-", r.start)
	}
	return fmt.Sprintf("bytes=%d-%d", r.start, r.end)
}

// bounds resolves the exact response range a compliant origin must return.
func (r legacyRangeRequest) bounds(total int64) (start, end int64, ok bool) {
	if total <= 0 {
		return 0, 0, false
	}
	if r.isSuffix() {
		start = total - r.suffix
		if start < 0 {
			start = 0
		}
		return start, total - 1, true
	}
	if r.start < 0 || r.start >= total {
		return 0, 0, false
	}
	end = r.end
	if end < 0 || end >= total {
		end = total - 1
	}
	if end < r.start {
		return 0, 0, false
	}
	return r.start, end, true
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

// parseLegacyRequestRange accepts one forward range or one suffix range. It
// rejects repeated and multipart Range values because they cannot be checked
// against a single Content-Range response without serving the wrong bytes.
func parseLegacyRequestRange(values []string) (legacyRangeRequest, bool, bool) {
	if len(values) == 0 {
		return legacyRangeRequest{}, false, true
	}
	if len(values) != 1 {
		return legacyRangeRequest{}, true, false
	}
	raw := strings.TrimSpace(values[0])
	lower := strings.ToLower(raw)
	if !strings.HasPrefix(lower, "bytes=") {
		return legacyRangeRequest{}, true, false
	}
	spec := strings.TrimSpace(raw[len("bytes="):])
	if spec == "" || strings.Contains(spec, ",") {
		return legacyRangeRequest{}, true, false
	}
	if strings.HasPrefix(spec, "-") {
		suffix, err := strconv.ParseInt(strings.TrimSpace(spec[1:]), 10, 64)
		if err != nil || suffix <= 0 {
			return legacyRangeRequest{}, true, false
		}
		return legacyRangeRequest{suffix: suffix}, true, true
	}
	requested, ok := parseRequestRange(raw)
	if !ok {
		return legacyRangeRequest{}, true, false
	}
	return legacyRangeRequest{
		start: requested.start,
		end:   requested.end,
	}, true, true
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
	w.Header().Set("X-Content-Type-Options", "nosniff")
	baseClient := client
	client = legacyProxyRedirectSafeClient(client)
	ctx := r.Context()
	requested, hasRange, rangeOK := parseLegacyRequestRange(r.Header.Values("Range"))
	if !rangeOK {
		logLegacyProxyEvent("invalid_range", legacyProxyLogHost(target), 0)
		fail(w, http.StatusBadRequest, "仅支持单段 Range 请求")
		return
	}
	ifRange := ""
	if hasRange {
		ifRangeValues := r.Header.Values("If-Range")
		if len(ifRangeValues) > 1 {
			logLegacyProxyEvent("invalid_if_range", legacyProxyLogHost(target), 0)
			fail(w, http.StatusBadRequest, "If-Range 请求无效")
			return
		}
		if len(ifRangeValues) == 1 {
			ifRange = strings.TrimSpace(ifRangeValues[0])
		}
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, target, nil)
	if err != nil {
		logLegacyProxyEvent("request_build_failed", legacyProxyLogHost(target), 0)
		fail(w, http.StatusInternalServerError, "源请求构造失败")
		return
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// Source headers may contain their own conditionals. Only copy the
	// client's Range and If-Range, and never forward If-Range on its own.
	req.Header.Del("Range")
	req.Header.Del("If-Range")
	req.Header.Set("Accept-Encoding", "identity")
	if hasRange {
		req.Header.Set("Range", requested.headerValue())
		if ifRange != "" {
			req.Header.Set("If-Range", ifRange)
		}
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "DoriyaMedia/0.1")
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			logLegacyProxyEvent("request_failed", req.URL.Hostname(), 0)
			fail(w, http.StatusBadGateway, "源请求失败")
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return
	}
	if resp == nil || resp.Body == nil {
		logLegacyProxyEvent("request_failed", req.URL.Hostname(), 0)
		fail(w, http.StatusBadGateway, "源请求失败")
		return
	}
	defer resp.Body.Close()

	if !proxyContentEncodingIsIdentity(resp.Header) {
		logLegacyProxyEvent("encoded_upstream", req.URL.Hostname(), resp.StatusCode)
		fail(w, http.StatusBadGateway, "源返回了压缩媒体响应")
		return
	}

	// The upstream ignored the range and is sending the whole file as 200.
	if hasRange && resp.StatusCode == http.StatusOK {
		// A 200 with If-Range can legitimately mean that the validator did not
		// match. Pass the full representation through instead of inventing a
		// 206 that the origin explicitly declined to send.
		if ifRange == "" {
			switch serveSlicedRange(w, r.Method, resp, requested) {
			case legacyRangeServed:
				return
			case legacyRangeUnsatisfiable:
				logLegacyProxyEvent("range_not_satisfiable", req.URL.Hostname(), resp.StatusCode)
				return
			default:
				logLegacyProxyEvent("ignored_range_no_length", req.URL.Hostname(), resp.StatusCode)
				fail(w, http.StatusBadGateway, "源返回了无法确定长度的分段响应")
				return
			}
		}
	}

	if resp.StatusCode == http.StatusPartialContent {
		start, end, total, length, ok := validateLegacyProxyPartial(resp, requested, hasRange)
		if !ok {
			logLegacyProxyEvent("unusable_content_range", req.URL.Hostname(), resp.StatusCode)
			fail(w, http.StatusBadGateway, "源返回了不一致的分段响应")
			return
		}
		if ifRange != "" && !proxyIfRangeMatchesResponse(ifRange, resp) {
			logLegacyProxyEvent("if_range_mismatch", req.URL.Hostname(), resp.StatusCode)
			fail(w, http.StatusBadGateway, "源返回了不一致的分段响应")
			return
		}
		for _, h := range []string{"Accept-Ranges", "Last-Modified", "ETag"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		setLegacyProxyMediaContentType(w, resp.Header.Get("Content-Type"))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusPartialContent)
		if r.Method != http.MethodHead {
			if n, err := io.CopyN(w, resp.Body, length); err != nil || n != length {
				abortLegacyProxyBodyCopy(req.URL.Hostname())
			}
		}
		return
	}

	// A HEAD without a known representation length is not useful to a media
	// player. If the source accepted HEAD but omitted the length, probe once and
	// fail closed rather than forwarding a 200 with no usable size.
	if r.Method == http.MethodHead && resp.StatusCode == http.StatusOK {
		if length, ok := legacyProxyContentLength(resp); !ok || length < 0 {
			if size, ranges, ok := probeSizeByRange(ctx, target, headers, baseClient); ok {
				if ranges != "" {
					w.Header().Set("Accept-Ranges", ranges)
				}
				setLegacyProxyMediaContentType(w, resp.Header.Get("Content-Type"))
				w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusOK)
				return
			}
			logLegacyProxyEvent("head_length_unknown", req.URL.Hostname(), resp.StatusCode)
			fail(w, http.StatusBadGateway, "源返回了无法确定长度的媒体响应")
			return
		}
	}

	if hasRange && resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		if ifRange != "" && !proxyIfRangeMatchesResponse(ifRange, resp) {
			logLegacyProxyEvent("if_range_mismatch", req.URL.Hostname(), resp.StatusCode)
			fail(w, http.StatusBadGateway, "源返回了不一致的分段响应")
			return
		}
		total, ok := legacyProxyUnsatisfiableTotal(resp)
		if !ok {
			total, _, ok = probeSizeByRange(ctx, target, headers, baseClient)
		}
		if !ok {
			logLegacyProxyEvent("unusable_content_range", req.URL.Hostname(), resp.StatusCode)
			fail(w, http.StatusBadGateway, "源返回了不一致的分段响应")
			return
		}
		if _, _, satisfiable := requested.bounds(total); satisfiable {
			logLegacyProxyEvent("unusable_content_range", req.URL.Hostname(), resp.StatusCode)
			fail(w, http.StatusBadGateway, "源返回了不一致的分段响应")
			return
		}
		logLegacyProxyEvent("range_not_satisfiable", req.URL.Hostname(), resp.StatusCode)
		writeLegacyProxyRangeUnsatisfiable(w, total)
		return
	}

	// HEAD that the upstream rejects: learn what we can with a one-byte GET so the
	// player still sees a length and range support. A valid Range 416 is handled
	// above and must not be converted into a full-representation 200.
	if r.Method == http.MethodHead && resp.StatusCode >= 400 {
		if size, ranges, ok := probeSizeByRange(ctx, target, headers, baseClient); ok {
			w.Header().Set("Accept-Ranges", ranges)
			setLegacyProxyMediaContentType(w, "")
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		logLegacyProxyEvent("upstream_error", req.URL.Hostname(), resp.StatusCode)
		fail(w, http.StatusBadGateway, "源媒体不可用")
		return
	}

	setLegacyProxyMediaContentType(w, resp.Header.Get("Content-Type"))
	for _, h := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
		if resp.StatusCode == http.StatusOK && h == "Content-Range" {
			continue
		}
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	expected, _ := legacyProxyContentLength(resp)
	written, err := io.Copy(w, resp.Body)
	if err != nil || (expected >= 0 && written != expected) {
		abortLegacyProxyBodyCopy(req.URL.Hostname())
	}
}

// legacyProxyRedirectSafeClient copies the client instead of mutating the
// shared source client. The copied client keeps its transport, cookies, timeout
// and any existing redirect policy, while enforcing the legacy proxy's origin
// and downgrade rules on every hop.
func legacyProxyRedirectSafeClient(client *http.Client) *http.Client {
	if client == nil {
		client = proxyClient
	}
	existing := client.CheckRedirect
	cloned := &http.Client{
		Transport: client.Transport,
		Jar:       client.Jar,
		Timeout:   client.Timeout,
	}
	cloned.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return fmt.Errorf("redirect chain has no previous request")
		}
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		previous := via[len(via)-1]
		if previous == nil || previous.URL == nil || req == nil || req.URL == nil {
			return fmt.Errorf("redirect has no usable URL")
		}
		scheme := strings.ToLower(req.URL.Scheme)
		if scheme != "http" && scheme != "https" {
			return fmt.Errorf("redirect uses an unsupported scheme")
		}
		if strings.EqualFold(previous.URL.Scheme, "https") && scheme == "http" {
			return fmt.Errorf("HTTPS redirect to HTTP is not allowed")
		}
		crossOrigin := !legacyProxySameOrigin(previous.URL, req.URL)
		// Go copies headers from the initial request on every redirect, not
		// just from the immediately preceding one. Once a chain has left the
		// source origin, keep cleaning every later hop as well.
		source := via[0]
		for _, hop := range via {
			if source == nil || source.URL == nil || hop == nil || hop.URL == nil ||
				!legacyProxySameOrigin(source.URL, hop.URL) {
				crossOrigin = true
			}
		}
		if crossOrigin {
			stripLegacyProxyRedirectHeaders(req.Header)
		}
		if existing != nil {
			if err := existing(req, via); err != nil {
				return err
			}
		}
		// A custom redirect policy must not be able to restore source headers on
		// a cross-origin hop.
		if crossOrigin {
			stripLegacyProxyRedirectHeaders(req.Header)
		}
		req.Header.Set("Accept-Encoding", "identity")
		return nil
	}
	return cloned
}

func legacyProxySameOrigin(a, b *url.URL) bool {
	left := legacyProxyOriginKey(a)
	return left != "" && left == legacyProxyOriginKey(b)
}

func legacyProxyOriginKey(u *url.URL) string {
	if u == nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return ""
		}
	}
	return scheme + "://" + host + ":" + port
}

func stripLegacyProxyRedirectHeaders(header http.Header) {
	preserved := make(http.Header, 4)
	for _, name := range []string{"Accept", "If-Range", "Range"} {
		for _, value := range header.Values(name) {
			preserved.Add(name, value)
		}
	}
	for name := range header {
		header.Del(name)
	}
	for name, values := range preserved {
		header[name] = values
	}
	header.Set("Accept-Encoding", "identity")
}

// legacyProxyLogHost extracts only the hostname before a request exists. The
// legacy proxy never logs the target URL, which may contain credentials.
func legacyProxyLogHost(target string) string {
	u, err := url.Parse(target)
	if err != nil || u == nil {
		return ""
	}
	return u.Hostname()
}

// logLegacyProxyEvent records only fixed fields controlled by this package.
func logLegacyProxyEvent(event, host string, status int) {
	if status > 0 {
		log.Printf("legacy media proxy: event=%s transport=legacy_proxy host=%q status=%d",
			event, host, status)
		return
	}
	log.Printf("legacy media proxy: event=%s transport=legacy_proxy host=%q",
		event, host)
}

type legacyRangeServeResult uint8

const (
	legacyRangeNotServed legacyRangeServeResult = iota
	legacyRangeServed
	legacyRangeUnsatisfiable
)

// serveSlicedRange answers a range request from a full-file 200 response by
// skipping the prefix and cutting the tail. It reports whether it served,
// could not safely serve, or found the requested start past EOF.
func serveSlicedRange(w http.ResponseWriter, method string, resp *http.Response, want legacyRangeRequest) legacyRangeServeResult {
	total, ok := legacyProxyContentLength(resp)
	if !ok {
		return legacyRangeNotServed
	}
	if total < 0 {
		return legacyRangeNotServed
	}
	start, end, ok := want.bounds(total)
	if !ok {
		writeLegacyProxyRangeUnsatisfiable(w, total)
		return legacyRangeUnsatisfiable
	}
	// Discard the prefix the client did not ask for. This is the cost of a source
	// that cannot do ranges itself.
	if method != http.MethodHead && start > 0 {
		if n, err := io.CopyN(io.Discard, resp.Body, start); err != nil || n != start {
			return legacyRangeNotServed
		}
	}
	length := end - start + 1
	setLegacyProxyMediaContentType(w, resp.Header.Get("Content-Type"))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusPartialContent)
	if method != http.MethodHead {
		if n, err := io.CopyN(w, resp.Body, length); err != nil || n != length {
			abortLegacyProxyBodyCopy("")
		}
	}
	return legacyRangeServed
}

func abortLegacyProxyBodyCopy(host string) {
	logLegacyProxyEvent("body_copy_failed", host, 0)
	panic(http.ErrAbortHandler)
}

// setLegacyProxyMediaContentType prevents a source from turning the
// same-origin stream endpoint into an active-content document. Only plain
// media containers, byte streams, and WebVTT are passed through by type.
func setLegacyProxyMediaContentType(w http.ResponseWriter, raw string) {
	w.Header().Set("Content-Type", safeLegacyProxyContentType(raw))
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func safeLegacyProxyContentType(raw string) string {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(raw))
	if err != nil {
		return "application/octet-stream"
	}
	mediaType = strings.ToLower(mediaType)
	if strings.HasPrefix(mediaType, "video/") || strings.HasPrefix(mediaType, "audio/") {
		return mediaType
	}
	switch mediaType {
	case "application/octet-stream",
		"application/mp4",
		"application/ogg",
		"application/x-matroska",
		"application/vnd.apple.mpegurl",
		"application/x-mpegurl",
		"application/mpegurl",
		"text/vtt":
		return mediaType
	default:
		return "application/octet-stream"
	}
}

func writeLegacyProxyRangeUnsatisfiable(w http.ResponseWriter, total int64) {
	w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
	w.Header().Set("Cache-Control", "no-store")
	fail(w, http.StatusRequestedRangeNotSatisfiable, "请求的分段不可满足")
}

func legacyProxyUnsatisfiableTotal(resp *http.Response) (int64, bool) {
	if resp == nil {
		return 0, false
	}
	values := resp.Header.Values("Content-Range")
	if len(values) != 1 {
		return 0, false
	}
	value := strings.TrimSpace(values[0])
	if !strings.HasPrefix(strings.ToLower(value), "bytes */") {
		return 0, false
	}
	total, err := strconv.ParseInt(
		strings.TrimSpace(value[len("bytes */"):]), 10, 64)
	if err != nil || total < 0 {
		return 0, false
	}
	return total, true
}

// legacyProxyContentLength returns -1 when the length is unknown. A malformed
// or internally inconsistent Content-Length fails the bool result.
func legacyProxyContentLength(resp *http.Response) (int64, bool) {
	if resp == nil {
		return -1, false
	}
	values := resp.Header.Values("Content-Length")
	if len(values) > 1 {
		return -1, false
	}
	length := int64(-1)
	if len(values) == 1 {
		value := strings.TrimSpace(values[0])
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			return -1, false
		}
		length = n
	}
	if resp.ContentLength > 0 {
		if length >= 0 && length != resp.ContentLength {
			return -1, false
		}
		length = resp.ContentLength
	}
	return length, true
}

func validateLegacyProxyPartial(resp *http.Response, want legacyRangeRequest, hasRange bool) (start, end, total, length int64, ok bool) {
	if !hasRange || resp == nil {
		return 0, 0, 0, 0, false
	}
	values := resp.Header.Values("Content-Range")
	if len(values) != 1 {
		return 0, 0, 0, 0, false
	}
	start, end, total, ok = parseContentRange(values[0])
	if !ok || total <= 0 || start < 0 || end < start || end >= total {
		return 0, 0, 0, 0, false
	}
	expectedStart, expectedEnd, ok := want.bounds(total)
	if !ok || start != expectedStart || end != expectedEnd {
		return 0, 0, 0, 0, false
	}
	length = end - start + 1
	claimed, ok := legacyProxyContentLength(resp)
	if !ok || (claimed >= 0 && claimed != length) {
		return 0, 0, 0, 0, false
	}
	return start, end, total, length, true
}

// probeSizeByRange asks for the first byte and reads the total out of the reply,
// for upstreams that refuse HEAD.
func probeSizeByRange(ctx context.Context, target string, headers map[string]string, client *http.Client) (int64, string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, "", false
	}
	client = legacyProxyRedirectSafeClient(client)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Del("Range")
	req.Header.Del("If-Range")
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("Accept-Encoding", "identity")
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "DoriyaMedia/0.1")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", false
	}
	if resp == nil || resp.Body == nil {
		return 0, "", false
	}
	defer resp.Body.Close()
	ranges := resp.Header.Get("Accept-Ranges")
	if !proxyContentEncodingIsIdentity(resp.Header) {
		return 0, ranges, false
	}
	if resp.StatusCode == http.StatusPartialContent {
		start, end, total, _, ok := validateLegacyProxyPartial(resp,
			legacyRangeRequest{start: 0, end: 0}, true)
		if !ok || start != 0 || end != 0 {
			return 0, ranges, false
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 2))
		if err != nil || len(body) != 1 {
			return 0, ranges, false
		}
		if ranges == "" {
			ranges = "bytes"
		}
		return total, ranges, true
	}
	if resp.StatusCode == http.StatusOK {
		if n, ok := legacyProxyContentLength(resp); ok && n >= 0 {
			return n, ranges, true
		}
	}
	return 0, ranges, false
}

// proxyContentEncodingIsIdentity accepts an absent or identity encoding only.
// Any transformation makes Content-Length and byte ranges describe different
// representations, so callers must not use such a response for sizing or
// serving bytes.
func proxyContentEncodingIsIdentity(header http.Header) bool {
	for _, value := range header.Values("Content-Encoding") {
		for _, encoding := range strings.Split(value, ",") {
			encoding = strings.TrimSpace(encoding)
			if encoding == "" || strings.EqualFold(encoding, "identity") {
				continue
			}
			return false
		}
	}
	return true
}
