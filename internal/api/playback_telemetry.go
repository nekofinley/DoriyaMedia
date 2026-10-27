package api

import (
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	playbackAssetStream   = "stream"
	playbackAssetPlaylist = "playlist"
	playbackAssetSegment  = "segment"
	playbackAssetSubtitle = "subtitle"

	playbackFailureStageComplete      = "complete"
	playbackFailureStageBody          = "body"
	playbackFailureStageRequest       = "request"
	playbackFailureStageAuthorization = "authorization"
	playbackFailureStageSession       = "session"
	playbackFailureStageRateLimit     = "rate_limit"
	playbackFailureStageSource        = "source"
	playbackFailureStageInternal      = "internal"
	playbackFailureStageResponse      = "response"
)

// playbackTelemetry records fixed, non-sensitive playback dimensions. It
// deliberately has no file ID, ticket, device, host, URL, query, or header
// fields: those values are useful for debugging but are exactly the identifiers
// that must not end up in media logs.
type playbackTelemetry struct {
	writer               http.ResponseWriter
	method               string
	transport            string
	surface              string
	asset                string
	status               int
	bytes                int64
	writeErr             bool
	failureStageOverride string
	started              time.Time
}

func newPlaybackTelemetry(
	w http.ResponseWriter,
	transport string,
	surface string,
	r *http.Request,
) *playbackTelemetry {
	method := ""
	if r != nil {
		method = r.Method
	}
	return &playbackTelemetry{
		writer:    w,
		method:    method,
		transport: transport,
		surface:   surface,
		asset:     playbackAssetStream,
		started:   time.Now(),
	}
}

func newV1PlaybackTelemetry(
	w http.ResponseWriter,
	transport string,
	r *http.Request,
) *playbackTelemetry {
	return newPlaybackTelemetry(w, transport, "v1", r)
}

func (t *playbackTelemetry) setAsset(asset string) {
	if t != nil {
		t.asset = safePlaybackTelemetryAsset(asset)
	}
}

func (t *playbackTelemetry) overrideFailureStage(stage string) {
	if t == nil {
		return
	}
	switch stage {
	case playbackFailureStageBody,
		playbackFailureStageRequest,
		playbackFailureStageAuthorization,
		playbackFailureStageSession,
		playbackFailureStageRateLimit,
		playbackFailureStageSource,
		playbackFailureStageInternal,
		playbackFailureStageResponse:
		t.failureStageOverride = stage
	}
}

func setPlaybackFailureStage(w http.ResponseWriter, stage string) {
	for depth := 0; w != nil && depth < 8; depth++ {
		if setter, ok := w.(interface{ overrideFailureStage(string) }); ok {
			setter.overrideFailureStage(stage)
			return
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		next := unwrapper.Unwrap()
		if next == w {
			return
		}
		w = next
	}
}

func (t *playbackTelemetry) Header() http.Header {
	return t.writer.Header()
}

func (t *playbackTelemetry) WriteHeader(status int) {
	if t.status != 0 {
		return
	}
	t.status = status
	t.writer.WriteHeader(status)
}

func (t *playbackTelemetry) Write(p []byte) (int, error) {
	if t.status == 0 {
		t.status = http.StatusOK
	}
	n, err := t.writer.Write(p)
	if t.method != http.MethodHead {
		if err != nil || n != len(p) {
			t.writeErr = true
		}
		t.bytes += int64(n)
	}
	return n, err
}

func (t *playbackTelemetry) ReadFrom(r io.Reader) (int64, error) {
	if t.status == 0 {
		t.status = http.StatusOK
	}
	var (
		n   int64
		err error
	)
	if readerFrom, ok := t.writer.(io.ReaderFrom); ok {
		n, err = readerFrom.ReadFrom(r)
	} else {
		n, err = io.Copy(t.writer, r)
	}
	if t.method != http.MethodHead {
		if err != nil {
			t.writeErr = true
		}
		t.bytes += n
	}
	return n, err
}

func (t *playbackTelemetry) Flush() {
	if flusher, ok := t.writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap lets http.ResponseController reach optional interfaces implemented by
// the real writer without this wrapper pretending to implement all of them.
func (t *playbackTelemetry) Unwrap() http.ResponseWriter {
	return t.writer
}

func (t *playbackTelemetry) finish() {
	if t == nil {
		return
	}
	asset := safePlaybackTelemetryAsset(t.asset)
	status := t.status
	if status == 0 {
		status = http.StatusOK
	}
	outcome := "success"
	failureStage := playbackFailureStageComplete
	// A 3xx is a successful transport handoff (notably direct HTTP), not a
	// media response: response_bytes may still describe its small body, while
	// only 2xx stream/segment responses contribute server_media_bytes.
	if status < 200 || status >= 400 {
		outcome = "error"
		failureStage = playbackFailureStageForStatus(status)
		if t.failureStageOverride != "" {
			failureStage = t.failureStageOverride
		}
	}
	isHead := t.method == http.MethodHead
	if !isHead {
		truncated := false
		if expected, ok := positiveContentLength(t.Header()); ok && t.bytes != expected {
			truncated = true
		}
		if t.writeErr || truncated {
			outcome = "aborted"
			failureStage = playbackFailureStageBody
		}
	}
	responseBytes := int64(0)
	if !isHead {
		responseBytes = t.bytes
	}
	serverMediaBytes := int64(0)
	if isPlaybackMediaAsset(asset) &&
		(status == http.StatusOK || status == http.StatusPartialContent) {
		serverMediaBytes = responseBytes
	}
	v1PlaybackTelemetryRecorder.record(
		t.surface, t.transport, asset, outcome, failureStage,
		responseBytes, serverMediaBytes)
	log.Printf(
		"playback media: event=media_response surface=%s transport=%s asset=%s outcome=%s failure_stage=%s status=%d response_bytes=%d server_media_bytes=%d duration_ms=%d",
		safePlaybackTelemetrySurface(t.surface),
		safePlaybackTelemetryTransport(t.transport),
		asset,
		safePlaybackTelemetryOutcome(outcome),
		safePlaybackTelemetryFailureStage(failureStage),
		status,
		responseBytes,
		serverMediaBytes,
		time.Since(t.started).Milliseconds(),
	)
}

func positiveContentLength(header http.Header) (int64, bool) {
	if header == nil {
		return 0, false
	}
	raw := strings.TrimSpace(header.Get("Content-Length"))
	if raw == "" {
		return 0, false
	}
	var value int64
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, false
		}
		value = value*10 + int64(raw[i]-'0')
		if value < 0 {
			return 0, false
		}
	}
	return value, true
}

func v1PlaybackFailureStage(status int) string {
	return playbackFailureStageForStatus(status)
}

func playbackFailureStageForStatus(status int) string {
	switch {
	case status == http.StatusBadRequest,
		status == http.StatusMethodNotAllowed,
		status == http.StatusRequestedRangeNotSatisfiable:
		return playbackFailureStageRequest
	case status == http.StatusUnauthorized,
		status == http.StatusForbidden:
		return playbackFailureStageAuthorization
	case status == http.StatusNotFound,
		status == http.StatusConflict,
		status == http.StatusGone:
		return playbackFailureStageSession
	case status == http.StatusTooManyRequests:
		return playbackFailureStageRateLimit
	case status == http.StatusUnsupportedMediaType,
		status == http.StatusBadGateway,
		status == http.StatusServiceUnavailable,
		status == http.StatusGatewayTimeout:
		return playbackFailureStageSource
	case status >= 500:
		return playbackFailureStageInternal
	default:
		return playbackFailureStageResponse
	}
}

func v1PlaybackFailureStageForError(status int, code V1ErrorCode) string {
	switch code {
	case V1ErrorInvalidRequest:
		return playbackFailureStageRequest
	case V1ErrorUnauthenticated,
		V1ErrorForbidden,
		V1ErrorFeatureDisabled:
		return playbackFailureStageAuthorization
	case V1ErrorMediaSessionNotFound,
		V1ErrorMediaSessionExpired,
		V1ErrorTicketNotFound,
		V1ErrorTicketExpired:
		return playbackFailureStageSession
	case V1ErrorRateLimited:
		return playbackFailureStageRateLimit
	case V1ErrorUnsupportedMedia,
		V1ErrorSourceUnavailable:
		return playbackFailureStageSource
	case V1ErrorInternal:
		return playbackFailureStageInternal
	default:
		return playbackFailureStageForStatus(status)
	}
}

func isPlaybackMediaAsset(asset string) bool {
	switch asset {
	case playbackAssetStream, playbackAssetSegment:
		return true
	default:
		return false
	}
}

func safePlaybackTelemetryAsset(value string) string {
	switch value {
	case playbackAssetStream,
		playbackAssetPlaylist,
		playbackAssetSegment,
		playbackAssetSubtitle:
		return value
	default:
		return playbackAssetStream
	}
}

func safePlaybackTelemetryTransport(value string) string {
	switch value {
	case "direct-http", "direct-dash", "server-proxy", "server-hls", "legacy-stream":
		return value
	default:
		return "unknown"
	}
}

func safePlaybackTelemetryOutcome(value string) string {
	switch value {
	case "success", "error", "aborted":
		return value
	default:
		return "unknown"
	}
}

func safePlaybackTelemetryFailureStage(value string) string {
	switch value {
	case playbackFailureStageComplete,
		playbackFailureStageBody,
		playbackFailureStageRequest,
		playbackFailureStageAuthorization,
		playbackFailureStageSession,
		playbackFailureStageRateLimit,
		playbackFailureStageSource,
		playbackFailureStageInternal,
		playbackFailureStageResponse:
		return value
	default:
		return "unknown"
	}
}

func safePlaybackTelemetrySurface(value string) string {
	switch value {
	case "v1", "legacy":
		return value
	default:
		return "unknown"
	}
}

type v1PlaybackTelemetryKey struct {
	Surface      string
	Transport    string
	Asset        string
	Outcome      string
	FailureStage string
}

type v1PlaybackTelemetryValue struct {
	Count            int64
	ResponseBytes    int64
	ServerMediaBytes int64
}

type v1PlaybackTelemetryStore struct {
	mu     sync.Mutex
	values map[v1PlaybackTelemetryKey]v1PlaybackTelemetryValue
}

func (s *v1PlaybackTelemetryStore) record(
	surface, transport, asset, outcome, failureStage string,
	responseBytes, serverMediaBytes int64,
) {
	if s == nil {
		return
	}
	key := v1PlaybackTelemetryKey{
		Surface:      safePlaybackTelemetrySurface(surface),
		Transport:    safePlaybackTelemetryTransport(transport),
		Asset:        safePlaybackTelemetryAsset(asset),
		Outcome:      safePlaybackTelemetryOutcome(outcome),
		FailureStage: safePlaybackTelemetryFailureStage(failureStage),
	}
	s.mu.Lock()
	if s.values == nil {
		s.values = map[v1PlaybackTelemetryKey]v1PlaybackTelemetryValue{}
	}
	value := s.values[key]
	value.Count++
	if responseBytes > 0 {
		value.ResponseBytes += responseBytes
	}
	if serverMediaBytes > 0 {
		value.ServerMediaBytes += serverMediaBytes
	}
	s.values[key] = value
	s.mu.Unlock()
}

func (s *v1PlaybackTelemetryStore) reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.values = nil
	s.mu.Unlock()
}

func (s *v1PlaybackTelemetryStore) snapshot() []struct {
	Key   v1PlaybackTelemetryKey
	Value v1PlaybackTelemetryValue
} {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]struct {
		Key   v1PlaybackTelemetryKey
		Value v1PlaybackTelemetryValue
	}, 0, len(s.values))
	for key, value := range s.values {
		out = append(out, struct {
			Key   v1PlaybackTelemetryKey
			Value v1PlaybackTelemetryValue
		}{Key: key, Value: value})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.Surface != out[j].Key.Surface {
			return out[i].Key.Surface < out[j].Key.Surface
		}
		if out[i].Key.Transport != out[j].Key.Transport {
			return out[i].Key.Transport < out[j].Key.Transport
		}
		if out[i].Key.Asset != out[j].Key.Asset {
			return out[i].Key.Asset < out[j].Key.Asset
		}
		if out[i].Key.Outcome != out[j].Key.Outcome {
			return out[i].Key.Outcome < out[j].Key.Outcome
		}
		return out[i].Key.FailureStage < out[j].Key.FailureStage
	})
	return out
}

var v1PlaybackTelemetryRecorder v1PlaybackTelemetryStore

func logV1PlaybackTransportIssued(
	transport string,
	requestFallbackCount int,
	remainingTransports int,
	ticketAttemptOrdinal int,
	lastClaimedTransport string,
) {
	if requestFallbackCount < 0 {
		requestFallbackCount = 0
	}
	if remainingTransports < 0 {
		remainingTransports = 0
	}
	if ticketAttemptOrdinal < 0 {
		ticketAttemptOrdinal = 0
	}
	previousTransport := "none"
	if lastClaimedTransport != "" {
		previousTransport = safePlaybackTelemetryTransport(lastClaimedTransport)
	}
	log.Printf(
		"v1 playback ticket: event=transport_issued transport=%s request_issuance_fallback_count=%d remaining_transports=%d ticket_attempt_ordinal=%d last_claimed_transport=%s",
		safePlaybackTelemetryTransport(transport),
		requestFallbackCount,
		remainingTransports,
		ticketAttemptOrdinal,
		previousTransport,
	)
}

func logV1PlaybackTransportAttemptFailed(
	transport string,
	failureStage string,
	requestFallbackCount int,
) {
	if requestFallbackCount < 0 {
		requestFallbackCount = 0
	}
	log.Printf(
		"v1 playback ticket: event=transport_attempt_failed transport=%s failure_stage=%s request_issuance_fallback_count=%d",
		safePlaybackTelemetryTransport(transport),
		safePlaybackTelemetryFailureStage(failureStage),
		requestFallbackCount,
	)
}
