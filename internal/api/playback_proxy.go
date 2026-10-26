package api

import (
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	v1ProxyCacheControl          = "private, no-store"
	v1ProxyErrorCacheControl     = "no-store"
	v1ProxySourceUnavailableMsg  = "The media source is unavailable."
	v1ProxyInternalErrorMsg      = "The server could not proxy the media."
	v1ProxyInvalidRangeMsg       = "Only a single forward byte range is supported."
	v1ProxyRangeUnsatisfiableMsg = "The requested byte range is not satisfiable."
	v1ProxyBadUpstreamMsg        = "The media source returned an invalid response."
)

// serveV1ProxyMedia transfers a server-proxy media object. Source credentials
// stay in headers and neither upstream errors nor response headers are exposed
// unless they are on the media response allowlist.
func serveV1ProxyMedia(w http.ResponseWriter, r *http.Request, target string, headers map[string]string, client *http.Client) {
	if r == nil {
		writeV1ProxyError(w, nil, http.StatusInternalServerError, V1ErrorInternal, v1ProxyInternalErrorMsg)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeV1ProxyError(w, r, http.StatusMethodNotAllowed, V1ErrorInvalidRequest, "Only GET and HEAD are supported.")
		return
	}

	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		log.Printf("v1 media proxy: invalid upstream target")
		writeV1ProxyError(w, r, http.StatusInternalServerError, V1ErrorInternal, v1ProxyInternalErrorMsg)
		return
	}
	upstreamHost := u.Hostname()

	rangeValues := r.Header.Values("Range")
	var wantRange byteRange
	var hasRange bool
	if len(rangeValues) > 1 {
		writeV1ProxyError(w, r, http.StatusBadRequest, V1ErrorInvalidRequest, v1ProxyInvalidRangeMsg)
		return
	}
	if len(rangeValues) == 1 {
		wantRange, hasRange = parseRequestRange(rangeValues[0])
		if !hasRange {
			writeV1ProxyError(w, r, http.StatusBadRequest, V1ErrorInvalidRequest, v1ProxyInvalidRangeMsg)
			return
		}
	}
	var ifRange string
	if hasRange {
		ifRangeValues := r.Header.Values("If-Range")
		if len(ifRangeValues) > 1 {
			writeV1ProxyError(w, r, http.StatusBadRequest, V1ErrorInvalidRequest, v1ProxyInvalidRangeMsg)
			return
		}
		if len(ifRangeValues) == 1 {
			ifRange = strings.TrimSpace(ifRangeValues[0])
		}
	}

	if client == nil {
		client = proxyClient
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, nil)
	if err != nil {
		log.Printf("v1 media proxy: could not build upstream request")
		writeV1ProxyError(w, r, http.StatusInternalServerError, V1ErrorInternal, v1ProxyInternalErrorMsg)
		return
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Del("Range")
	req.Header.Del("If-Range")
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "DoriyaMedia/0.1")
	}
	// Prevent transparent gzip decoding from changing the bytes and making the
	// source's Content-Length and Range semantics disagree.
	req.Header.Set("Accept-Encoding", "identity")
	if hasRange {
		req.Header.Set("Range", formatV1ProxyRange(wantRange))
		if ifRange != "" {
			req.Header.Set("If-Range", ifRange)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		logV1ProxyUpstreamFailure(upstreamHost, 0)
		writeV1ProxyError(w, r, http.StatusBadGateway, V1ErrorSourceUnavailable, v1ProxySourceUnavailableMsg)
		return
	}
	if resp == nil || resp.Body == nil {
		logV1ProxyUpstreamFailure(upstreamHost, 0)
		writeV1ProxyError(w, r, http.StatusBadGateway, V1ErrorSourceUnavailable, v1ProxySourceUnavailableMsg)
		return
	}
	defer resp.Body.Close()

	if !proxyContentEncodingIsIdentity(resp.Header) {
		logV1ProxyUpstreamFailure(upstreamHost, resp.StatusCode)
		writeV1ProxyError(w, r, http.StatusBadGateway, V1ErrorSourceUnavailable, v1ProxyBadUpstreamMsg)
		return
	}

	switch resp.StatusCode {
	case http.StatusOK:
		if _, ok := legacyProxyContentLength(resp); !ok {
			logV1ProxyUpstreamFailure(upstreamHost, resp.StatusCode)
			writeV1ProxyError(w, r, http.StatusBadGateway, V1ErrorSourceUnavailable, v1ProxyBadUpstreamMsg)
			return
		}
		// With If-Range, an upstream 200 can mean the validator did not match.
		// Passing the representation through is safer than inventing a range.
		if hasRange && ifRange != "" {
			if r.Method == http.MethodHead && v1ProxyContentLength(resp) < 0 {
				if total, _, ok := probeSizeByRange(r.Context(), target, headers, client); ok {
					serveV1ProxyHeadProbe(w, r, resp, total, byteRange{}, false)
					return
				}
				logV1ProxyUpstreamFailure(upstreamHost, resp.StatusCode)
				writeV1ProxyError(w, r, http.StatusBadGateway, V1ErrorSourceUnavailable, v1ProxySourceUnavailableMsg)
				return
			}
			serveV1ProxyFull(w, r, resp, upstreamHost)
			return
		}
		if hasRange {
			if serveV1ProxyIgnoredRange(w, r, resp, wantRange, upstreamHost) {
				return
			}
			if r.Method == http.MethodHead {
				if total, _, ok := probeSizeByRange(r.Context(), target, headers, client); ok {
					serveV1ProxyHeadProbe(w, r, resp, total, wantRange, hasRange)
					return
				}
			}
			logV1ProxyUpstreamFailure(upstreamHost, resp.StatusCode)
			writeV1ProxyError(w, r, http.StatusBadGateway, V1ErrorSourceUnavailable, v1ProxySourceUnavailableMsg)
			return
		}
		if r.Method == http.MethodHead && v1ProxyContentLength(resp) < 0 {
			if total, _, ok := probeSizeByRange(r.Context(), target, headers, client); ok {
				serveV1ProxyHeadProbe(w, r, resp, total, wantRange, hasRange)
				return
			}
			logV1ProxyUpstreamFailure(upstreamHost, resp.StatusCode)
			writeV1ProxyError(w, r, http.StatusBadGateway, V1ErrorSourceUnavailable, v1ProxySourceUnavailableMsg)
			return
		}
		serveV1ProxyFull(w, r, resp, upstreamHost)
		return
	case http.StatusPartialContent:
		if serveV1ProxyPartial(w, r, resp, wantRange, hasRange, ifRange, upstreamHost) {
			return
		}
		logV1ProxyUpstreamFailure(upstreamHost, resp.StatusCode)
		writeV1ProxyError(w, r, http.StatusBadGateway, V1ErrorSourceUnavailable, v1ProxyBadUpstreamMsg)
		return
	case http.StatusRequestedRangeNotSatisfiable:
		if hasRange && ifRange != "" && !proxyIfRangeMatchesResponse(ifRange, resp) {
			logV1ProxyUpstreamFailure(upstreamHost, resp.StatusCode)
			writeV1ProxyError(w, r, http.StatusBadGateway, V1ErrorSourceUnavailable, v1ProxyBadUpstreamMsg)
			return
		}
		contentRangeValues := resp.Header.Values("Content-Range")
		if len(contentRangeValues) > 1 {
			logV1ProxyUpstreamFailure(upstreamHost, resp.StatusCode)
			writeV1ProxyError(w, r, http.StatusBadGateway, V1ErrorSourceUnavailable, v1ProxyBadUpstreamMsg)
			return
		}
		total, ok := int64(0), false
		if len(contentRangeValues) == 1 {
			total, ok = parseV1ProxyUnsatisfiedContentRange(contentRangeValues[0])
		}
		if !hasRange {
			ok = false
		}
		if !ok && hasRange {
			total, _, ok = probeSizeByRange(r.Context(), target, headers, client)
		}
		if !ok || wantRange.start < total {
			logV1ProxyUpstreamFailure(upstreamHost, resp.StatusCode)
			writeV1ProxyError(w, r, http.StatusBadGateway, V1ErrorSourceUnavailable, v1ProxyBadUpstreamMsg)
			return
		}
		writeV1ProxyRangeUnsatisfiable(w, r, total)
		return
	}

	// Some sources reject HEAD even though a one-byte ranged GET can still tell
	// the client the media length. Only do this after the source has failed the
	// HEAD request, and never expose the source's error response.
	if r.Method == http.MethodHead && resp.StatusCode >= http.StatusBadRequest {
		if total, _, ok := probeSizeByRange(r.Context(), target, headers, client); ok {
			serveV1ProxyHeadProbe(w, r, resp, total, wantRange, hasRange && ifRange == "")
			return
		}
	}

	logV1ProxyUpstreamFailure(upstreamHost, resp.StatusCode)
	writeV1ProxyError(w, r, http.StatusBadGateway, V1ErrorSourceUnavailable, v1ProxySourceUnavailableMsg)
}

func formatV1ProxyRange(want byteRange) string {
	if want.end < 0 {
		return fmt.Sprintf("bytes=%d-", want.start)
	}
	return fmt.Sprintf("bytes=%d-%d", want.start, want.end)
}

func serveV1ProxyFull(w http.ResponseWriter, r *http.Request, resp *http.Response, upstreamHost string) {
	copyV1ProxyMediaHeaders(w, resp)
	w.Header().Del("Content-Range")
	w.Header().Set("Cache-Control", v1ProxyCacheControl)
	if length := v1ProxyContentLength(resp); length >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.Header().Set("Accept-Ranges", "bytes")
	} else {
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	written, err := io.Copy(w, resp.Body)
	if err != nil {
		abortV1ProxyBodyCopy(upstreamHost)
	}
	if expected := v1ProxyContentLength(resp); expected > 0 && written != expected {
		abortV1ProxyBodyCopy(upstreamHost)
	}
}

func serveV1ProxyPartial(
	w http.ResponseWriter,
	r *http.Request,
	resp *http.Response,
	want byteRange,
	hasRange bool,
	ifRange string,
	upstreamHost string,
) bool {
	contentRangeValues := resp.Header.Values("Content-Range")
	if !hasRange || len(contentRangeValues) != 1 {
		return false
	}
	start, end, total, ok := parseContentRange(contentRangeValues[0])
	if !ok || total < 0 || start < 0 || end < start || end >= total {
		return false
	}
	expectedEnd := total - 1
	if want.end >= 0 && want.end < total {
		expectedEnd = want.end
	}
	if start != want.start || end != expectedEnd {
		return false
	}
	length := end - start + 1
	claimed, ok := legacyProxyContentLength(resp)
	if !ok || (claimed >= 0 && claimed != length) {
		return false
	}
	if ifRange != "" && !proxyIfRangeMatchesResponse(ifRange, resp) {
		return false
	}

	copyV1ProxyMediaHeaders(w, resp)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", v1ProxyCacheControl)
	w.WriteHeader(http.StatusPartialContent)
	if r.Method == http.MethodHead {
		return true
	}
	if n, err := io.CopyN(w, resp.Body, length); err != nil || n != length {
		abortV1ProxyBodyCopy(upstreamHost)
	}
	return true
}

// serveV1ProxyIgnoredRange answers a range from a full 200 response when the
// upstream has a trustworthy content length. It returns false when the bytes
// cannot be sliced safely.
func serveV1ProxyIgnoredRange(w http.ResponseWriter, r *http.Request, resp *http.Response, want byteRange, upstreamHost string) bool {
	total := v1ProxyContentLength(resp)
	if total < 0 {
		return false
	}
	if want.start >= total {
		writeV1ProxyRangeUnsatisfiable(w, r, total)
		return true
	}
	if r.Method == http.MethodHead {
		end := want.end
		if end < 0 || end >= total {
			end = total - 1
		}
		copyV1ProxyMediaHeaders(w, resp)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", want.start, end, total))
		w.Header().Set("Content-Length", strconv.FormatInt(end-want.start+1, 10))
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Cache-Control", v1ProxyCacheControl)
		w.WriteHeader(http.StatusPartialContent)
		return true
	}

	if !serveV1ProxySlicedRange(w, resp, want, total, upstreamHost) {
		return false
	}
	return true
}

// serveV1ProxySlicedRange is the v1-only equivalent of serveSlicedRange. It
// never forwards the source content type, and it aborts the downstream
// connection if the source ends before the advertised representation does.
func serveV1ProxySlicedRange(
	w http.ResponseWriter,
	resp *http.Response,
	want byteRange,
	total int64,
	upstreamHost string,
) bool {
	if want.start < 0 || want.start >= total {
		return false
	}
	if want.start > 0 {
		n, err := io.CopyN(io.Discard, resp.Body, want.start)
		if err != nil || n != want.start {
			return false
		}
	}

	end := want.end
	if end < 0 || end >= total {
		end = total - 1
	}
	length := end - want.start + 1
	copyV1ProxyMediaHeaders(w, resp)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", want.start, end, total))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", v1ProxyCacheControl)
	w.WriteHeader(http.StatusPartialContent)
	if n, err := io.CopyN(w, resp.Body, length); err != nil || n != length {
		abortV1ProxyBodyCopy(upstreamHost)
	}
	return true
}

func serveV1ProxyHeadProbe(w http.ResponseWriter, r *http.Request, resp *http.Response, total int64, want byteRange, hasRange bool) {
	copyV1ProxyMediaHeaders(w, resp)
	w.Header().Set("Cache-Control", v1ProxyCacheControl)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Del("Content-Range")
	if hasRange {
		if want.start >= total {
			writeV1ProxyRangeUnsatisfiable(w, r, total)
			return
		}
		end := want.end
		if end < 0 || end >= total {
			end = total - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", want.start, end, total))
		w.Header().Set("Content-Length", strconv.FormatInt(end-want.start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
	w.WriteHeader(http.StatusOK)
}

func writeV1ProxyRangeUnsatisfiable(w http.ResponseWriter, r *http.Request, total int64) {
	w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
	writeV1ProxyError(w, r, http.StatusRequestedRangeNotSatisfiable, V1ErrorInvalidRequest, v1ProxyRangeUnsatisfiableMsg)
}

func writeV1ProxyError(w http.ResponseWriter, r *http.Request, status int, code V1ErrorCode, message string) {
	for _, name := range []string{"Content-Type", "Content-Length", "Accept-Ranges", "Last-Modified", "ETag"} {
		w.Header().Del(name)
	}
	if status != http.StatusRequestedRangeNotSatisfiable {
		w.Header().Del("Content-Range")
	}
	w.Header().Set("Cache-Control", v1ProxyErrorCacheControl)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r != nil && r.Method == http.MethodHead {
		writeV1Error(v1ProxyHeadWriter{ResponseWriter: w}, status, code, message, nil)
		return
	}
	writeV1Error(w, status, code, message, nil)
}

func copyV1ProxyMediaHeaders(w http.ResponseWriter, resp *http.Response) {
	for _, name := range []string{
		"Content-Length",
		"Content-Range",
		"Accept-Ranges",
		"Last-Modified",
		"ETag",
	} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.Header().Set("Content-Type", safeV1ProxyContentType(resp.Header.Get("Content-Type")))
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func safeV1ProxyContentType(raw string) string {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(raw))
	if err != nil {
		return "application/octet-stream"
	}
	mediaType = strings.ToLower(mediaType)
	if strings.HasPrefix(mediaType, "video/") ||
		strings.HasPrefix(mediaType, "audio/") {
		return mediaType
	}
	switch mediaType {
	case "application/octet-stream",
		"application/mp4",
		"application/ogg",
		"application/x-matroska",
		"application/vnd.apple.mpegurl",
		"application/dash+xml":
		return mediaType
	default:
		return "application/octet-stream"
	}
}

func v1ProxyContentLength(resp *http.Response) int64 {
	if resp == nil {
		return -1
	}
	values := resp.Header.Values("Content-Length")
	if len(values) > 1 {
		return -1
	}
	if len(values) == 1 {
		n, err := strconv.ParseInt(strings.TrimSpace(values[0]), 10, 64)
		if err != nil || n < 0 {
			return -1
		}
		if resp.ContentLength > 0 && resp.ContentLength != n {
			return -1
		}
		return n
	}
	if resp.ContentLength > 0 {
		return resp.ContentLength
	}
	if resp.ContentLength == 0 {
		return 0
	}
	return -1
}

// proxyIfRangeMatchesResponse rejects a 206 when the response carries a
// validator that explicitly conflicts with the client's If-Range condition.
// Sources that omit the relevant validator are trusted to have evaluated the
// condition themselves.
func proxyIfRangeMatchesResponse(ifRange string, resp *http.Response) bool {
	ifRange = strings.TrimSpace(ifRange)
	if resp == nil || ifRange == "" {
		return false
	}
	if strings.HasPrefix(ifRange, `"`) {
		etags := resp.Header.Values("ETag")
		if len(etags) == 0 {
			return true
		}
		return len(etags) == 1 && strings.TrimSpace(etags[0]) == ifRange
	}
	validator, err := http.ParseTime(ifRange)
	if err != nil {
		return false
	}
	lastModified := resp.Header.Values("Last-Modified")
	if len(lastModified) == 0 {
		return true
	}
	if len(lastModified) != 1 {
		return false
	}
	modified, err := http.ParseTime(lastModified[0])
	if err != nil {
		return false
	}
	return modified.UTC().Truncate(time.Second).Equal(validator.UTC().Truncate(time.Second))
}

func parseV1ProxyUnsatisfiedContentRange(value string) (int64, bool) {
	value = strings.TrimSpace(value)
	const prefix = "bytes */"
	if !strings.HasPrefix(strings.ToLower(value), prefix) {
		return 0, false
	}
	raw := strings.TrimSpace(value[len(prefix):])
	if raw == "" {
		return 0, false
	}
	total, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || total < 0 {
		return 0, false
	}
	return total, true
}

func logV1ProxyUpstreamFailure(host string, status int) {
	if status != 0 {
		log.Printf("v1 media proxy: upstream status=%d host=%s", status, host)
		return
	}
	log.Printf("v1 media proxy: upstream request failed host=%s", host)
}

func logV1ProxyBodyFailure(host string) {
	log.Printf("v1 media proxy: upstream body copy failed host=%s", host)
}

func abortV1ProxyBodyCopy(host string) {
	logV1ProxyBodyFailure(host)
	panic(http.ErrAbortHandler)
}

// v1ProxyHeadWriter suppresses the JSON error body for HEAD while still using
// the shared v1 error encoder.
type v1ProxyHeadWriter struct {
	http.ResponseWriter
}

func (w v1ProxyHeadWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

func (w v1ProxyHeadWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
