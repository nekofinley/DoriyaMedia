package api

import (
	"context"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"doriyamedia/internal/db"
	"doriyamedia/internal/media"
	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

const v1PlaybackSubtitleMaxBytes = 8 << 20

var (
	v1PlaybackSubtitleCapabilityPattern = regexp.MustCompile(`^ss_[A-Za-z0-9_-]{22,96}$`)
	v1PlaybackSubtitleLanguagePattern   = regexp.MustCompile(`^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$`)
)

// snapshotV1PlaybackSubtitleIDs discovers the same embedded, external, and
// driver-published tracks the legacy player sees, then snapshots only their local
// ids. No path, URL, title, or header is copied into ticket storage.
func (s *Server) snapshotV1PlaybackSubtitleIDs(
	ctx context.Context,
	file *models.MediaFile,
	drv source.Driver,
) []int64 {
	if s == nil || s.Store == nil || s.Engine == nil || file == nil || drv == nil {
		return []int64{}
	}
	seen := make(map[int64]struct{})
	ids := make([]int64, 0, 8)
	for _, sub := range s.discoverSubtitles(ctx, drv, file) {
		if sub.ID <= 0 || sub.FileID != file.ID {
			continue
		}
		if _, _, ok := v1PlaybackSubtitleDescription(&sub); !ok {
			continue
		}
		if _, duplicate := seen[sub.ID]; duplicate {
			continue
		}
		seen[sub.ID] = struct{}{}
		ids = append(ids, sub.ID)
		if len(ids) == models.PlaybackMaxSubtitleGrants {
			break
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// issueV1PlaybackSubtitles creates one independently revocable capability per
// deliverable text track. Bitmap tracks are described without a URL, except for
// server-HLS where they are omitted because S3 does not expose a burn path.
func (s *Server) issueV1PlaybackSubtitles(
	ticket *models.PlaybackTicket,
	file *models.MediaFile,
	transport string,
	mediaCapability string,
	startSeconds float64,
	baseURL string,
) ([]v1PlaybackSubtitle, error) {
	if s == nil || s.Store == nil || ticket == nil || file == nil ||
		len(ticket.SubtitleIDs) == 0 {
		return nil, nil
	}
	if startSeconds < 0 || math.IsNaN(startSeconds) || math.IsInf(startSeconds, 0) {
		return nil, db.ErrInvalidPlaybackSubtitleSession
	}
	baseURL = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")

	all, err := s.Store.SubtitlesByFile(file.ID)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]models.Subtitle, len(all))
	for _, sub := range all {
		byID[sub.ID] = sub
	}
	granted := make(map[int64]struct{}, len(ticket.SubtitleIDs))
	for _, id := range ticket.SubtitleIDs {
		granted[id] = struct{}{}
	}

	type selectedSubtitle struct {
		sub         models.Subtitle
		format      string
		deliverable bool
	}
	selected := make([]selectedSubtitle, 0, len(ticket.SubtitleIDs))
	servable := make([]models.Subtitle, 0, len(ticket.SubtitleIDs))
	for _, sub := range all {
		if _, ok := granted[sub.ID]; !ok || sub.FileID != file.ID {
			continue
		}
		format, deliverable, ok := v1PlaybackSubtitleDescription(&sub)
		if !ok || (transport == models.PlaybackTransportServerHLS && !deliverable) {
			continue
		}
		selected = append(selected, selectedSubtitle{
			sub: sub, format: format, deliverable: deliverable,
		})
		if deliverable {
			servable = append(servable, sub)
		}
	}
	if len(selected) == 0 {
		return nil, nil
	}
	defaultID := media.PickDefaultSubtitle(servable)

	out := make([]v1PlaybackSubtitle, 0, len(selected))
	createdGrant := false
	issued := false
	defer func() {
		if !issued && createdGrant {
			s.cleanupV1PlaybackSubtitleIssuance(ticket, transport)
		}
	}()
	for _, item := range selected {
		url := ""
		if item.deliverable {
			grant, err := s.Store.CreatePlaybackSubtitleSession(
				models.PlaybackSubtitleSessionRequest{
					Ticket:       playbackBinding(ticket),
					Transport:    transport,
					SubtitleID:   item.sub.ID,
					StartSeconds: startSeconds,
				},
				time.Now().UTC(),
			)
			if err != nil {
				return nil, err
			}
			createdGrant = true
			url = v1PlaybackSubtitleURL(
				baseURL, mediaCapability, transport, grant.Capability)
			if url == "" {
				return nil, db.ErrInvalidPlaybackSubtitleSession
			}
		}
		out = append(out, v1PlaybackSubtitle{
			ID:       "sub_" + strconv.FormatInt(item.sub.ID, 10),
			Language: v1PlaybackSubtitleLanguage(item.sub.Lang),
			Label:    v1PlaybackSubtitleLabel(&item.sub),
			Format:   item.format,
			URL:      url,
			Default:  item.sub.ID == defaultID,
			Forced:   v1PlaybackSubtitleForced(&item.sub),
		})
	}
	issued = true
	return out, nil
}

func (s *Server) cleanupV1PlaybackSubtitleIssuance(
	ticket *models.PlaybackTicket,
	transport string,
) {
	if s == nil || s.Store == nil || ticket == nil {
		return
	}
	if _, err := s.Store.RevokePlaybackSubtitleSessionsForTicket(
		playbackBinding(ticket), transport, time.Now().UTC()); err != nil {
		log.Printf("playback subtitle cleanup failed transport=%s", transport)
	}
}

func v1PlaybackSubtitleURL(
	baseURL, mediaCapability, transport, subtitleCapability string,
) string {
	if _, ok := v1PlaybackSubtitleCapability(subtitleCapability); baseURL == "" || !ok {
		return ""
	}
	if transport == models.PlaybackTransportServerHLS {
		if _, ok := v1PlaybackCapability(mediaCapability); !ok {
			return ""
		}
		return baseURL + "/api/v1/playback/media-sessions/" + mediaCapability +
			"/subtitle-sessions/" + subtitleCapability + ".vtt"
	}
	return baseURL + "/api/v1/playback/subtitle-sessions/" +
		subtitleCapability + "/subtitle.vtt"
}

func v1PlaybackSubtitleDescription(sub *models.Subtitle) (string, bool, bool) {
	if sub == nil {
		return "", false, false
	}
	format := strings.ToLower(strings.TrimSpace(sub.Format))
	switch format {
	case "hdmv_pgs_subtitle", "pgs", "dvd_subtitle", "dvdsub",
		"dvb_subtitle", "dvbsub", "xsub":
		return "pgs", false, true
	case "subrip", "srt", "ass", "ssa", "webvtt", "vtt",
		"mov_text", "text":
		return "vtt", true, true
	case "":
		if sub.Convertible {
			return "vtt", true, true
		}
	}
	if sub.Convertible {
		return "vtt", true, true
	}
	return "", false, false
}

func v1PlaybackSubtitleLanguage(raw string) string {
	lang := strings.TrimSpace(raw)
	if len(lang) <= 32 && v1PlaybackSubtitleLanguagePattern.MatchString(lang) {
		return lang
	}
	return "und"
}

func v1PlaybackSubtitleLabel(sub *models.Subtitle) string {
	label := strings.TrimSpace(sub.Title)
	if label == "" {
		label = strings.TrimSpace(sub.Lang)
	}
	if label == "" {
		label = "Subtitle " + strconv.FormatInt(sub.ID, 10)
	}
	label = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' {
			return ' '
		}
		return r
	}, label)
	label = strings.Join(strings.Fields(label), " ")
	runes := []rune(label)
	if len(runes) > 128 {
		label = string(runes[:128])
	}
	if strings.TrimSpace(label) == "" {
		return "Subtitle " + strconv.FormatInt(sub.ID, 10)
	}
	return label
}

func v1PlaybackSubtitleForced(sub *models.Subtitle) bool {
	if sub == nil {
		return false
	}
	value := strings.ToLower(sub.Title + " " + sub.Path)
	return strings.Contains(value, "forced")
}

func v1PlaybackSubtitleCapability(value string) (string, bool) {
	if !v1PlaybackSubtitleCapabilityPattern.MatchString(value) {
		return "", false
	}
	return value, true
}

func (s *Server) handleV1PlaybackSubtitle(w http.ResponseWriter, r *http.Request) {
	s.serveV1PlaybackSubtitle(w, r, false)
}

func (s *Server) handleV1PlaybackHLSSubtitle(w http.ResponseWriter, r *http.Request) {
	s.serveV1PlaybackSubtitle(w, r, true)
}

func (s *Server) serveV1PlaybackSubtitle(
	w http.ResponseWriter,
	r *http.Request,
	nested bool,
) {
	if r != nil && r.Method == http.MethodHead {
		w = v1ProxyHeadWriter{ResponseWriter: w}
	}
	if s == nil || s.Cfg == nil || !s.Cfg.Client.V1Enabled ||
		!s.Cfg.Client.DeviceAuthEnabled || !s.Cfg.Client.PlaybackTicketEnabled {
		s.revokeV1PlaybackAllMediaSessions()
		writeV1PlaybackError(w, http.StatusNotFound, V1ErrorFeatureDisabled,
			"The playback media feature is disabled.", nil)
		return
	}
	if r == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
		writeV1PlaybackError(w, http.StatusMethodNotAllowed, V1ErrorInvalidRequest,
			"Only GET and HEAD are supported.", nil)
		return
	}
	if r.URL == nil || r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeV1PlaybackError(w, http.StatusBadRequest, V1ErrorInvalidRequest,
			"Subtitle-session URLs do not accept query parameters.", nil)
		return
	}
	s.revokeDisabledV1PlaybackSubtitleSessions()

	var (
		mediaSession *db.PlaybackMediaSessionAccess
		session      *db.PlaybackSubtitleSessionAccess
		ok           bool
	)
	if nested {
		if !s.Cfg.Client.ServerHLSV1Enabled {
			s.revokeV1PlaybackMediaSessionsForTransport(models.PlaybackTransportServerHLS)
			writeV1TransportDisabled(w, "The requested playback transport is disabled.")
			return
		}
		mediaCapability, valid := v1PlaybackCapability(r.PathValue("media"))
		if !valid {
			writeV1PlaybackError(w, http.StatusNotFound, V1ErrorMediaSessionNotFound,
				"The media session was not found.", nil)
			return
		}
		mediaSession, ok = s.authorizeV1PlaybackMediaSession(
			w, r, mediaCapability, models.PlaybackTransportServerHLS)
		if !ok {
			return
		}
	}

	raw := r.PathValue("id")
	if nested {
		raw = strings.TrimSuffix(r.PathValue("sub"), ".vtt")
	}
	capability, valid := v1PlaybackSubtitleCapability(raw)
	if !valid {
		writeV1PlaybackError(w, http.StatusNotFound, V1ErrorMediaSessionNotFound,
			"The subtitle session was not found.", nil)
		return
	}
	session, ok = s.authorizeV1PlaybackSubtitleSession(w, r, capability)
	if !ok {
		return
	}
	if nested {
		if mediaSession == nil || mediaSession.Session == nil ||
			mediaSession.Session.TicketID != session.Session.TicketID ||
			mediaSession.Session.UserID != session.Session.UserID ||
			mediaSession.Session.DeviceID != session.Session.DeviceID ||
			mediaSession.Session.FileID != session.Session.FileID ||
			session.Session.Transport != models.PlaybackTransportServerHLS {
			writeV1PlaybackError(w, http.StatusNotFound, V1ErrorMediaSessionNotFound,
				"The subtitle session was not found.", nil)
			return
		}
	} else if session.Session.Transport == models.PlaybackTransportServerHLS {
		// HLS text renditions are issued only under the media capability so a
		// subtitle capability cannot become an independent HLS bypass.
		writeV1PlaybackError(w, http.StatusNotFound, V1ErrorMediaSessionNotFound,
			"The subtitle session was not found.", nil)
		return
	}
	if !s.v1PlaybackSubtitleTransportEnabled(session.Session.Transport) {
		s.revokeV1PlaybackSubtitleSessionsForTransport(session.Session.Transport)
		writeV1PlaybackError(w, http.StatusForbidden, V1ErrorTransportDisabled,
			"The requested playback transport is disabled.", nil)
		return
	}

	sub, err := s.Store.SubtitleByID(session.Session.SubtitleID)
	if err != nil || sub == nil || sub.FileID != session.Session.FileID {
		writeV1PlaybackError(w, http.StatusNotFound, V1ErrorMediaSessionNotFound,
			"The subtitle session was not found.", nil)
		return
	}
	file, drv, err := s.v1PlaybackFileAndDriver(session.Session.FileID)
	if err != nil {
		writeV1PlaybackError(w, http.StatusBadGateway, V1ErrorSourceUnavailable,
			"The media source is unavailable.", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	data, ok := s.loadV1PlaybackSubtitleVTT(ctx, file, drv, sub)
	if !ok {
		writeV1PlaybackError(w, http.StatusBadGateway, V1ErrorSourceUnavailable,
			"The subtitle source is unavailable.", nil)
		return
	}
	data = media.ShiftVTT(data, session.Session.StartSeconds)
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func (s *Server) authorizeV1PlaybackSubtitleSession(
	w http.ResponseWriter,
	r *http.Request,
	capability string,
) (*db.PlaybackSubtitleSessionAccess, bool) {
	requireEmailVerification := s.Cfg != nil && s.Cfg.EmailVerificationRequired()
	for attempts := 0; attempts < 4; attempts++ {
		access, err := s.Store.AuthorizePlaybackSubtitleSession(
			capability, requireEmailVerification, time.Now().UTC())
		if err == nil {
			if access == nil || access.Session == nil ||
				access.Session.State != models.PlaybackSubtitleSessionActive {
				writeV1PlaybackError(w, http.StatusNotFound, V1ErrorMediaSessionNotFound,
					"The subtitle session was not found.", nil)
				return nil, false
			}
			return access, true
		}
		if errors.Is(err, db.ErrPlaybackSubtitleSessionConflict) && attempts < 3 {
			select {
			case <-r.Context().Done():
				return nil, false
			case <-time.After(time.Duration(attempts+1) * 5 * time.Millisecond):
			}
			continue
		}
		writeV1PlaybackSubtitleSessionError(w, err)
		return nil, false
	}
	writeV1PlaybackError(w, http.StatusConflict, V1ErrorInvalidRequest,
		"The subtitle session is busy; retry the request.", nil)
	return nil, false
}

func (s *Server) loadV1PlaybackSubtitleVTT(
	ctx context.Context,
	file *models.MediaFile,
	drv source.Driver,
	sub *models.Subtitle,
) ([]byte, bool) {
	if s == nil || s.Engine == nil || file == nil || drv == nil || sub == nil {
		return nil, false
	}
	if sub.Embedded {
		data, err := s.Engine.ExtractEmbeddedSubtitle(ctx, file, drv, sub.StreamIdx)
		if err != nil || len(data) == 0 {
			return nil, false
		}
		return data, true
	}

	var (
		rc  io.ReadCloser
		err error
	)
	if ss, ok := drv.(source.SubtitleSource); ok {
		rc, err = ss.OpenSubtitle(ctx, file.Path, sub.Path)
	} else {
		rc, err = openSourceRead(ctx, drv, sub.Path)
	}
	if err != nil || rc == nil {
		return nil, false
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, v1PlaybackSubtitleMaxBytes+1))
	if err != nil || len(raw) > v1PlaybackSubtitleMaxBytes {
		return nil, false
	}
	format, _ := s.subFormatFor(ctx, drv, sub)
	data := media.ToVTT(format, raw)
	if len(data) == 0 {
		return nil, false
	}
	return data, true
}

func writeV1PlaybackSubtitleSessionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrPlaybackSubtitleSessionNotFound):
		writeV1PlaybackError(w, http.StatusNotFound, V1ErrorMediaSessionNotFound,
			"The subtitle session was not found.", nil)
	case errors.Is(err, db.ErrPlaybackSubtitleSessionExpired):
		writeV1PlaybackError(w, http.StatusGone, V1ErrorMediaSessionExpired,
			"The subtitle session has expired.", nil)
	case errors.Is(err, db.ErrPlaybackSubtitleSessionRevoked):
		writeV1PlaybackError(w, http.StatusForbidden, V1ErrorTransportDisabled,
			"The subtitle session is no longer available.", nil)
	case errors.Is(err, db.ErrPlaybackTicketExpired):
		writeV1PlaybackError(w, http.StatusGone, V1ErrorTicketExpired,
			"The playback ticket has expired.", nil)
	case errors.Is(err, db.ErrPlaybackTicketRevoked):
		writeV1PlaybackError(w, http.StatusGone, V1ErrorTicketExpired,
			"The playback ticket has expired.", nil)
	case errors.Is(err, db.ErrPlaybackTicketNotFound):
		writeV1PlaybackError(w, http.StatusNotFound, V1ErrorTicketNotFound,
			"The playback ticket was not found.", nil)
	case errors.Is(err, db.ErrDevicePlaybackDisabled):
		writeV1DeviceFeatureDisabled(w)
	case errors.Is(err, db.ErrDeviceRevoked):
		writeV1PlaybackError(w, http.StatusForbidden, V1ErrorForbidden,
			"The device session is no longer valid.", nil)
	case errors.Is(err, db.ErrDeviceUserUnavailable):
		writeV1PlaybackError(w, http.StatusForbidden, V1ErrorForbidden,
			"The account cannot play media.", nil)
	default:
		writeV1PlaybackError(w, http.StatusInternalServerError, V1ErrorInternal,
			"The server could not authorize the subtitle session.", nil)
	}
}

func (s *Server) v1PlaybackSubtitleTransportEnabled(transport string) bool {
	if s == nil || s.Cfg == nil {
		return false
	}
	switch transport {
	case models.PlaybackTransportDirectHTTP:
		return s.Cfg.Client.DirectHTTPEnabled
	case models.PlaybackTransportServerProxy:
		return s.Cfg.Client.ServerProxyV1Enabled
	case models.PlaybackTransportServerHLS:
		return s.Cfg.Client.ServerHLSV1Enabled
	default:
		return false
	}
}

func (s *Server) revokeDisabledV1PlaybackSubtitleSessions() {
	if s == nil || s.Cfg == nil {
		return
	}
	for _, item := range []struct {
		enabled   bool
		transport string
	}{
		{s.Cfg.Client.DirectHTTPEnabled, models.PlaybackTransportDirectHTTP},
		{s.Cfg.Client.ServerProxyV1Enabled, models.PlaybackTransportServerProxy},
		{s.Cfg.Client.ServerHLSV1Enabled, models.PlaybackTransportServerHLS},
	} {
		if !item.enabled {
			s.revokeV1PlaybackSubtitleSessionsForTransport(item.transport)
		}
	}
}

type v1PlaybackSubtitleSessionRevoker interface {
	RevokePlaybackSubtitleSessionsForTransport(string, time.Time) (int64, error)
	RevokeAllPlaybackSubtitleSessions(time.Time) (int64, error)
}

func (s *Server) revokeV1PlaybackSubtitleSessionsForTransport(transport string) {
	if s == nil {
		return
	}
	revokeV1PlaybackSubtitleSessionsForTransport(s.Store, transport)
}

func revokeV1PlaybackSubtitleSessionsForTransport(store any, transport string) {
	revoker, ok := store.(v1PlaybackSubtitleSessionRevoker)
	if !ok {
		return
	}
	if _, err := revoker.RevokePlaybackSubtitleSessionsForTransport(
		transport, time.Now().UTC()); err != nil {
		// Errors can contain storage connection details, never log them.
		log.Printf("playback subtitle session cleanup failed transport=%s", transport)
	}
}

func (s *Server) revokeAllV1PlaybackSubtitleSessions() {
	if s == nil {
		return
	}
	revokeAllV1PlaybackSubtitleSessionsFromStore(s.Store)
}

func revokeAllV1PlaybackSubtitleSessionsFromStore(store any) {
	revoker, ok := store.(v1PlaybackSubtitleSessionRevoker)
	if !ok {
		return
	}
	if _, err := revoker.RevokeAllPlaybackSubtitleSessions(time.Now().UTC()); err != nil {
		// Fail closed on the current request; keep DB details out of logs.
		log.Printf("playback subtitle session global cleanup failed")
	}
}
