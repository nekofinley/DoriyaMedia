package api

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"doriyamedia/internal/config"
	"doriyamedia/internal/db"
	"doriyamedia/internal/media"
	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

const (
	v1HLSSegmentSeconds         = 4.0
	v1HLSDefaultTranscodeHeight = 1080
	v1HLSAuthorizeAttempts      = 3
	v1HLSAuthorizeBackoff       = 35 * time.Millisecond
	v1HLSPlaylistContentType    = "application/vnd.apple.mpegurl"
	v1HLSSegmentContentType     = "video/mp2t"
)

var v1HLSCapabilityPattern = regexp.MustCompile(`^ms_[A-Za-z0-9_-]{22,96}$`)

// The handler depends on narrow interfaces so authorization, playlist shape,
// and segment arguments can be tested without a real database or ffmpeg.
type v1PlaybackHLSStore interface {
	AuthorizePlaybackMediaSessionContext(context.Context, string, bool, time.Time) (*db.PlaybackMediaSessionAccess, error)
	RevokePlaybackMediaSessionsForTransport(string, time.Time) (int64, error)
	FileByID(int64) (*models.MediaFile, error)
}

type v1PlaybackHLSEngine interface {
	DriverFor(*models.MediaFile) (source.Driver, error)
	EnsureProbe(context.Context, *models.MediaFile, source.Driver) (*media.Probe, error)
	Playlist(string, string, float64, float64) string
	Segment(context.Context, *models.MediaFile, source.Driver,
		float64, float64, string, string, int, int, int, io.Writer) (int64, error)
}

type v1ServerHLS struct {
	store             v1PlaybackHLSStore
	engine            v1PlaybackHLSEngine
	cfg               *config.Config
	now               func() time.Time
	authorizeAttempts int
	authorizeBackoff  time.Duration
}

// handleV1ServerHLSPlaylist serves the capability-addressed media playlist.
// The route belongs in server.go; keeping it out of this file limits the S3
// change to the HLS implementation and its tests.
func (s *Server) handleV1ServerHLSPlaylist(w http.ResponseWriter, r *http.Request) {
	s.v1ServerHLSHandler().servePlaylist(w, r)
}

// handleV1ServerHLSSegment serves one numbered MPEG-TS segment.
func (s *Server) handleV1ServerHLSSegment(w http.ResponseWriter, r *http.Request) {
	s.v1ServerHLSHandler().serveSegment(w, r)
}

func (s *Server) v1ServerHLSHandler() *v1ServerHLS {
	h := &v1ServerHLS{
		now:               time.Now,
		authorizeAttempts: v1HLSAuthorizeAttempts,
		authorizeBackoff:  v1HLSAuthorizeBackoff,
	}
	if s == nil {
		return h
	}
	h.cfg = s.Cfg
	if s.Store != nil {
		h.store = s.Store
	}
	if s.Engine != nil {
		h.engine = s.Engine
	}
	return h
}

func (h *v1ServerHLS) servePlaylist(w http.ResponseWriter, r *http.Request) {
	telemetry := newV1PlaybackTelemetry(w, models.PlaybackTransportServerHLS, r)
	telemetry.setAsset(playbackAssetPlaylist)
	defer telemetry.finish()
	w = telemetry
	if !h.prepareRequest(w, r) {
		return
	}
	capability, ok := v1HLSCapabilityFromPath(r)
	if !ok {
		writeV1HLSError(w, http.StatusNotFound, V1ErrorMediaSessionNotFound,
			"The media session was not found.", map[string]any{"action": "request_new_ticket"})
		return
	}

	session, ok := h.authorize(w, r.Context(), capability)
	if !ok {
		return
	}
	if !v1HLSSessionHasFrozenVideo(w, session) {
		return
	}
	if session.StartSeconds < 0 || math.IsNaN(session.StartSeconds) || math.IsInf(session.StartSeconds, 0) {
		writeV1HLSError(w, http.StatusInternalServerError, V1ErrorInternal,
			"The server could not prepare the media playlist.", nil)
		return
	}

	_, _, probe, ok := h.prepareMedia(w, r.Context(), session)
	if !ok {
		return
	}
	if probe.Video() == nil {
		writeV1HLSError(w, http.StatusUnsupportedMediaType, V1ErrorUnsupportedMedia,
			"The media has no playable video stream.", nil)
		return
	}
	duration := probe.Duration()
	if duration <= 0 {
		writeV1HLSError(w, http.StatusBadGateway, V1ErrorSourceUnavailable,
			"The media source is unavailable.", nil)
		return
	}
	if session.StartSeconds >= duration {
		writeV1HLSError(w, http.StatusNotFound, V1ErrorInvalidRequest,
			"The requested media playlist does not exist.", nil)
		return
	}

	basePath := "/api/v1/playback/media-sessions/" + capability
	playlist := h.engine.Playlist(basePath, ".ts", duration-session.StartSeconds, v1HLSSegmentSeconds)

	w.Header().Set("Content-Type", v1HLSPlaylistContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(playlist)))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, playlist)
}

func (h *v1ServerHLS) serveSegment(w http.ResponseWriter, r *http.Request) {
	telemetry := newV1PlaybackTelemetry(w, models.PlaybackTransportServerHLS, r)
	telemetry.setAsset(playbackAssetSegment)
	defer telemetry.finish()
	w = telemetry
	if !h.prepareRequest(w, r) {
		return
	}
	capability, ok := v1HLSCapabilityFromPath(r)
	if !ok {
		writeV1HLSError(w, http.StatusNotFound, V1ErrorMediaSessionNotFound,
			"The media session was not found.", map[string]any{"action": "request_new_ticket"})
		return
	}
	index, ok := v1HLSSegmentIndex(r.PathValue("segment"))
	if !ok {
		writeV1HLSError(w, http.StatusBadRequest, V1ErrorInvalidRequest,
			"The HLS segment name is invalid.", nil)
		return
	}

	session, ok := h.authorize(w, r.Context(), capability)
	if !ok {
		return
	}
	if !v1HLSSessionHasFrozenVideo(w, session) {
		return
	}
	videoMode, audioMode, ok := v1HLSEncodeModes(session.Mode)
	if !ok {
		writeV1TransportDisabled(w, "The media session mode is not supported.")
		return
	}

	file, driver, probe, ok := h.prepareMedia(w, r.Context(), session)
	if !ok {
		return
	}
	if probe.Video() == nil {
		writeV1HLSError(w, http.StatusUnsupportedMediaType, V1ErrorUnsupportedMedia,
			"The media has no playable video stream.", nil)
		return
	}
	duration := probe.Duration()
	if duration <= 0 {
		writeV1HLSError(w, http.StatusBadGateway, V1ErrorSourceUnavailable,
			"The media source is unavailable.", nil)
		return
	}
	start, ok := v1HLSSegmentStart(session.StartSeconds, index)
	if !ok || start >= duration {
		writeV1HLSError(w, http.StatusNotFound, V1ErrorInvalidRequest,
			"The requested HLS segment does not exist.", nil)
		return
	}
	if !v1HLSProbeHasVideoIndex(probe, session.VideoIndex) {
		writeV1HLSError(w, http.StatusBadGateway, V1ErrorSourceUnavailable,
			"The media source changed and no longer contains the authorized video stream.",
			map[string]any{"action": "request_new_ticket"})
		return
	}

	w.Header().Set("Content-Type", v1HLSSegmentContentType)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	n, err := h.engine.Segment(r.Context(), file, driver, start, v1HLSSegmentSeconds,
		videoMode, audioMode, session.AudioOrdinal, session.VideoIndex,
		v1HLSTranscodeHeight(session.TranscodeHeight), w)
	if err != nil || n == 0 {
		// Segment buffers its output until ffmpeg succeeds, so no media bytes
		// have been committed when this branch is reached.
		writeV1HLSError(w, http.StatusBadGateway, V1ErrorSourceUnavailable,
			"The media source is unavailable.", nil)
	}
}

func (h *v1ServerHLS) prepareRequest(w http.ResponseWriter, r *http.Request) bool {
	if h == nil || h.cfg == nil || !h.cfg.Client.V1Enabled ||
		!h.cfg.Client.DeviceAuthEnabled || !h.cfg.Client.PlaybackTicketEnabled {
		h.revokeAllSessions()
		writeV1HLSError(w, http.StatusNotFound, V1ErrorFeatureDisabled,
			"The playback media feature is disabled.", nil)
		return false
	}
	if !h.cfg.Client.ServerHLSV1Enabled {
		h.revokeTransportSessions()
		writeV1TransportDisabled(w, "The requested playback transport is disabled.")
		return false
	}
	if r == nil || r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeV1HLSError(w, http.StatusMethodNotAllowed, V1ErrorInvalidRequest,
			"Only GET is supported.", nil)
		return false
	}
	if v1HLSRequestHasQuery(r) {
		writeV1HLSError(w, http.StatusBadRequest, V1ErrorInvalidRequest,
			"Media session requests do not accept query parameters.", nil)
		return false
	}
	if h.store == nil || h.engine == nil {
		writeV1HLSError(w, http.StatusInternalServerError, V1ErrorInternal,
			"The server could not complete the request.", nil)
		return false
	}
	return true
}

func (h *v1ServerHLS) revokeTransportSessions() {
	if h == nil || h.store == nil {
		return
	}
	if _, err := h.store.RevokePlaybackMediaSessionsForTransport(
		models.PlaybackTransportServerHLS, time.Now().UTC()); err != nil {
		// Never log the underlying DB error: it can include source paths or
		// connection details. The response already fails closed.
		log.Printf("playback HLS media session cleanup failed")
	}
	revokeV1PlaybackSubtitleSessionsForTransport(
		h.store, models.PlaybackTransportServerHLS)
}

func (h *v1ServerHLS) revokeAllSessions() {
	if h == nil || h.store == nil {
		return
	}
	for _, transport := range []string{
		models.PlaybackTransportServerProxy,
		models.PlaybackTransportServerHLS,
	} {
		if _, err := h.store.RevokePlaybackMediaSessionsForTransport(
			transport, time.Now().UTC()); err != nil {
			log.Printf("playback media session cleanup failed transport=%s", transport)
		}
		revokeV1PlaybackSubtitleSessionsForTransport(h.store, transport)
	}
	revokeAllV1PlaybackSubtitleSessionsFromStore(h.store)
}

// authorize reauthorizes and touches the media session for every playlist and
// segment request. CAS conflicts are retried a small, bounded number of times.
func (h *v1ServerHLS) authorize(w http.ResponseWriter, ctx context.Context, capability string) (*models.PlaybackMediaSession, bool) {
	attempts := h.authorizeAttempts
	if attempts <= 0 {
		attempts = v1HLSAuthorizeAttempts
	}
	now := h.now
	if now == nil {
		now = time.Now
	}

	var (
		access *db.PlaybackMediaSessionAccess
		err    error
	)
	for attempt := 0; attempt < attempts; attempt++ {
		access, err = h.store.AuthorizePlaybackMediaSessionContext(
			ctx, capability, h.cfg.EmailVerificationRequired(), now())
		if !errors.Is(err, db.ErrPlaybackMediaSessionConflict) {
			break
		}
		if attempt+1 == attempts || h.authorizeBackoff <= 0 {
			continue
		}
		wait := h.authorizeBackoff * time.Duration(1<<attempt)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			err = ctx.Err()
			attempt = attempts
		case <-timer.C:
		}
	}
	if err != nil {
		writeV1HLSAuthorizeError(w, err)
		return nil, false
	}
	if access == nil || access.Session == nil {
		writeV1HLSError(w, http.StatusInternalServerError, V1ErrorInternal,
			"The server could not authorize the media session.", nil)
		return nil, false
	}
	if access.Session.Transport != models.PlaybackTransportServerHLS {
		writeV1TransportDisabled(w, "The requested playback transport is disabled.")
		return nil, false
	}
	return access.Session, true
}

func (h *v1ServerHLS) prepareMedia(w http.ResponseWriter, ctx context.Context, session *models.PlaybackMediaSession) (*models.MediaFile, source.Driver, *media.Probe, bool) {
	file, err := h.store.FileByID(session.FileID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeV1HLSError(w, http.StatusNotFound, V1ErrorMediaSessionNotFound,
				"The media session was not found.", map[string]any{"action": "request_new_ticket"})
		} else {
			writeV1HLSError(w, http.StatusInternalServerError, V1ErrorInternal,
				"The server could not load the media.", nil)
		}
		return nil, nil, nil, false
	}
	driver, err := h.engine.DriverFor(file)
	if err != nil {
		writeV1HLSError(w, http.StatusBadGateway, V1ErrorSourceUnavailable,
			"The media source is unavailable.", nil)
		return nil, nil, nil, false
	}
	probe, err := h.engine.EnsureProbe(ctx, file, driver)
	if err != nil || probe == nil {
		writeV1HLSError(w, http.StatusBadGateway, V1ErrorSourceUnavailable,
			"The media source is unavailable.", nil)
		return nil, nil, nil, false
	}
	return file, driver, probe, true
}

func writeV1HLSAuthorizeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrPlaybackMediaSessionNotFound):
		writeV1HLSError(w, http.StatusNotFound, V1ErrorMediaSessionNotFound,
			"The media session was not found.", map[string]any{"action": "request_new_ticket"})
	case errors.Is(err, db.ErrPlaybackMediaSessionExpired):
		writeV1HLSError(w, http.StatusGone, V1ErrorMediaSessionExpired,
			"The media session has expired.", map[string]any{"action": "request_new_ticket"})
	case errors.Is(err, db.ErrPlaybackMediaSessionRevoked):
		writeV1HLSError(w, http.StatusForbidden, V1ErrorTransportDisabled,
			"The media session is no longer available.", map[string]any{"action": "request_new_ticket"})
	case errors.Is(err, db.ErrDevicePlaybackDisabled):
		writeV1HLSError(w, http.StatusForbidden, V1ErrorFeatureDisabled,
			"This device feature is disabled.", map[string]any{"action": "contact_admin"})
	case errors.Is(err, db.ErrPlaybackTicketExpired),
		errors.Is(err, db.ErrPlaybackTicketRevoked):
		writeV1HLSError(w, http.StatusGone, V1ErrorTicketExpired,
			"The playback ticket has expired.", map[string]any{"action": "request_new_ticket"})
	case errors.Is(err, db.ErrPlaybackTicketNotFound):
		writeV1HLSError(w, http.StatusNotFound, V1ErrorTicketNotFound,
			"The playback ticket was not found.", map[string]any{"action": "request_new_ticket"})
	case errors.Is(err, db.ErrDeviceRevoked):
		writeV1HLSError(w, http.StatusForbidden, V1ErrorForbidden,
			"The device session is no longer valid.", map[string]any{"action": "reauth"})
	case errors.Is(err, db.ErrDeviceUserUnavailable):
		writeV1HLSError(w, http.StatusForbidden, V1ErrorForbidden,
			"The account cannot play media.", nil)
	default:
		writeV1HLSError(w, http.StatusInternalServerError, V1ErrorInternal,
			"The server could not authorize the media session.", nil)
	}
}

func writeV1TransportDisabled(w http.ResponseWriter, message string) {
	writeV1HLSError(w, http.StatusForbidden, V1ErrorTransportDisabled, message, map[string]any{
		"transport": models.PlaybackTransportServerHLS,
		"action":    "request_new_ticket",
	})
}

func writeV1HLSError(w http.ResponseWriter, status int, code V1ErrorCode, message string, details map[string]any) {
	setPlaybackFailureStage(w, v1PlaybackFailureStageForError(status, code))
	for _, name := range []string{
		"Content-Type", "Content-Length", "Content-Range", "Content-Disposition",
		"Accept-Ranges", "Last-Modified", "ETag",
	} {
		w.Header().Del(name)
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writeV1Error(w, status, code, message, details)
}

func v1HLSCapabilityFromPath(r *http.Request) (string, bool) {
	if r == nil {
		return "", false
	}
	capability := r.PathValue("id")
	if !v1HLSCapabilityPattern.MatchString(capability) {
		return "", false
	}
	return capability, true
}

func v1HLSRequestHasQuery(r *http.Request) bool {
	return r != nil && r.URL != nil && (r.URL.RawQuery != "" || r.URL.ForceQuery)
}

func v1HLSSegmentIndex(segment string) (int, bool) {
	if !strings.HasSuffix(segment, ".ts") {
		return 0, false
	}
	digits := strings.TrimSuffix(segment, ".ts")
	if digits == "" {
		return 0, false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	index, err := strconv.Atoi(digits)
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

func v1HLSSegmentStart(sessionStart float64, index int) (float64, bool) {
	if index < 0 || sessionStart < 0 ||
		math.IsNaN(sessionStart) || math.IsInf(sessionStart, 0) {
		return 0, false
	}
	offset := float64(index) * v1HLSSegmentSeconds
	if offset > math.MaxFloat64-sessionStart {
		return 0, false
	}
	start := sessionStart + offset
	if math.IsNaN(start) || math.IsInf(start, 0) || start < sessionStart {
		return 0, false
	}
	return start, true
}

func v1HLSEncodeModes(mode string) (string, string, bool) {
	switch mode {
	case "remux":
		return "copy", "copy", true
	case "remux-aac":
		return "copy", "aac", true
	case "transcode":
		return "transcode", "aac", true
	default:
		return "", "", false
	}
}

func v1HLSTranscodeHeight(height *int) int {
	if height == nil || *height <= 0 || *height > 16384 {
		return v1HLSDefaultTranscodeHeight
	}
	return *height
}

func v1HLSSessionHasFrozenVideo(w http.ResponseWriter, session *models.PlaybackMediaSession) bool {
	if session != nil && session.VideoIndex >= 0 {
		return true
	}
	writeV1HLSError(w, http.StatusGone, V1ErrorMediaSessionExpired,
		"The media session predates frozen playback parameters; request a new ticket.",
		map[string]any{"action": "request_new_ticket"})
	return false
}

func v1HLSProbeHasVideoIndex(probe *media.Probe, index int) bool {
	if probe == nil || index < 0 {
		return false
	}
	for i := range probe.Streams {
		stream := &probe.Streams[i]
		if stream.Index == index && stream.CodecType == "video" && !stream.IsCoverArt() {
			return true
		}
	}
	return false
}
