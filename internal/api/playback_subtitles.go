package api

import (
	"context"
	"encoding/json"
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
	"doriyamedia/internal/scan"
	"doriyamedia/internal/source"
)

const (
	v1PlaybackSubtitleMaxBytes    = 8 << 20
	v1PlaybackSubtitleListTimeout = 5 * time.Second
)

type playbackProbeSubtitleKey struct {
	path        string
	streamIndex int
}

var (
	v1PlaybackSubtitleCapabilityPattern = regexp.MustCompile(`^ss_[A-Za-z0-9_-]{22,96}$`)
	v1PlaybackSubtitleLanguagePattern   = regexp.MustCompile(`^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$`)
)

// prepareV1PlaybackProbeMetadata performs playback-ticket probe and subtitle
// discovery as pure reads. The returned rows are committed with the ticket CAS,
// so a revocation that lands during discovery leaves no cache side effects.
func (s *Server) prepareV1PlaybackProbeMetadata(
	ctx context.Context,
	file *models.MediaFile,
	drv source.Driver,
	probe *media.Probe,
) (db.PlaybackTicketProbeMetadata, error) {
	return s.prepareV1PlaybackProbeMetadataWithTimeout(
		ctx, file, drv, probe, v1PlaybackSubtitleListTimeout)
}

func (s *Server) prepareV1PlaybackProbeMetadataWithTimeout(
	ctx context.Context,
	file *models.MediaFile,
	drv source.Driver,
	probe *media.Probe,
	listTimeout time.Duration,
) (db.PlaybackTicketProbeMetadata, error) {
	if s == nil || file == nil || probe == nil {
		return db.PlaybackTicketProbeMetadata{}, errors.New(
			"playback probe metadata unavailable")
	}
	rawProbe, err := json.Marshal(probe)
	if err != nil {
		return db.PlaybackTicketProbeMetadata{}, err
	}
	metadata := db.PlaybackTicketProbeMetadata{
		FileProbe: string(rawProbe),
		FileVersion: &db.PlaybackTicketFileVersion{
			Size: file.Size, MTime: file.MTime,
		},
	}
	candidates := make([]models.Subtitle, 0, 8)
	seen := make(map[playbackProbeSubtitleKey]int, 8)
	add := func(sub models.Subtitle) {
		sub.FileID = file.ID
		if _, _, ok := v1PlaybackSubtitleDescription(&sub); !ok {
			return
		}
		key := playbackProbeSubtitleKey{
			path: sub.Path, streamIndex: sub.StreamIdx,
		}
		if index, duplicate := seen[key]; duplicate {
			candidates[index] = sub
			return
		}
		seen[key] = len(candidates)
		candidates = append(candidates, sub)
	}

	if ss, ok := drv.(source.SubtitleSource); ok {
		tracks, listErr := boundedPlaybackMetadataCall(
			ctx, listTimeout,
			func(callCtx context.Context) ([]source.SubtitleTrack, error) {
				return ss.Subtitles(callCtx, file.Path)
			},
		)
		if err := ctx.Err(); err != nil {
			return db.PlaybackTicketProbeMetadata{}, err
		}
		// External discovery is best-effort. A list error, including this
		// helper's own watchdog deadline, only omits external tracks; the
		// probe-derived metadata and ticket finalization still proceed.
		if listErr == nil {
			for _, track := range tracks {
				if track.Path == "" {
					continue
				}
				add(models.Subtitle{
					Path: track.Path, Lang: track.Lang, Title: track.Title,
					Format: track.Format, Convertible: track.Convertible,
				})
			}
		}
	} else if drv != nil && !strings.HasPrefix(file.Path, "http://") &&
		!strings.HasPrefix(file.Path, "https://") {
		if dir, base := splitDirBase(file.Path); dir != "" && base != "" {
			entries, listErr := boundedPlaybackMetadataCall(
				ctx, listTimeout,
				func(callCtx context.Context) ([]source.Entry, error) {
					return drv.List(callCtx, dir)
				},
			)
			if err := ctx.Err(); err != nil {
				return db.PlaybackTicketProbeMetadata{}, err
			}
			// As with SubtitleSource, a directory-listing failure is not a
			// reason to fail playback-ticket finalization.
			if listErr == nil {
				names := make([]string, len(entries))
				for i, entry := range entries {
					if entry.IsDir {
						continue
					}
					if _, ok := subFormatOf(entry.Name); ok {
						names[i] = entry.Name
					}
				}
				attached := scan.SubtitleAttachmentIn(base, names)
				for i, entry := range entries {
					if !attached[i] {
						continue
					}
					format, convertible := subFormatOf(entry.Name)
					add(models.Subtitle{
						Path: entry.Path, Lang: media.GuessLang(entry.Name),
						Title: media.SubLabel(entry.Name), Format: format,
						Convertible: convertible,
					})
				}
			}
		}
	}
	for _, sub := range media.EmbeddedSubtitleRows(file.ID, probe) {
		add(sub)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Path != candidates[j].Path {
			return candidates[i].Path < candidates[j].Path
		}
		if candidates[i].StreamIdx != candidates[j].StreamIdx {
			return candidates[i].StreamIdx < candidates[j].StreamIdx
		}
		return !candidates[i].Embedded && candidates[j].Embedded
	})
	if len(candidates) > models.PlaybackMaxSubtitleGrants {
		candidates = candidates[:models.PlaybackMaxSubtitleGrants]
	}
	metadata.Subtitles = candidates
	return metadata, nil
}

// boundedPlaybackMetadataCall lets finalization stop waiting when a source
// driver ignores cancellation. The driver goroutine is not forcibly stopped,
// but it can no longer hold the ticket transaction or HTTP handler open.
//
// When only the internal watchdog expires, it returns context.DeadlineExceeded
// while ctx remains live. Best-effort callers intentionally ignore that result;
// cancellation or expiry of ctx itself is returned instead and remains fatal.
func boundedPlaybackMetadataCall[T any](
	ctx context.Context,
	timeout time.Duration,
	call func(context.Context) (T, error),
) (T, error) {
	var zero T
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := call(callCtx)
		done <- result{value: value, err: err}
	}()
	select {
	case got := <-done:
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		return got.value, got.err
	case <-callCtx.Done():
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		return zero, context.DeadlineExceeded
	}
}

// issueV1PlaybackSubtitles creates one independently revocable capability per
// deliverable text track. Bitmap tracks are described without a URL, except for
// server-HLS where they are omitted because S3 does not expose a burn path.
func (s *Server) issueV1PlaybackSubtitles(
	ctx context.Context,
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
			s.cleanupV1PlaybackSubtitleIssuance(ctx, ticket, transport)
		}
	}()
	for _, item := range selected {
		url := ""
		if item.deliverable {
			grant, err := s.Store.CreatePlaybackSubtitleSessionContext(
				ctx,
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
	parent context.Context,
	ticket *models.PlaybackTicket,
	transport string,
) {
	if s == nil || s.Store == nil || ticket == nil {
		return
	}
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(parent), v1PlaybackCleanupTimeout)
	defer cancel()
	if _, err := s.Store.RevokePlaybackSubtitleSessionsForTicketContext(
		ctx,
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
	telemetry := newV1PlaybackTelemetry(w, "", r)
	telemetry.setAsset(playbackAssetSubtitle)
	if nested {
		// The nested route is always an HLS rendition. Flat subtitle routes can
		// belong to either direct-http or server-proxy, so their transport is
		// resolved from the authorized session below.
		telemetry.transport = models.PlaybackTransportServerHLS
	}
	defer telemetry.finish()
	w = telemetry
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
	telemetry.transport = session.Session.Transport
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
		if err := r.Context().Err(); err != nil {
			writeV1PlaybackSubtitleSessionError(w, err)
			return nil, false
		}
		access, err := s.Store.AuthorizePlaybackSubtitleSessionContext(
			r.Context(), capability, requireEmailVerification, time.Now().UTC())
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
				writeV1PlaybackSubtitleSessionError(w, r.Context().Err())
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
	case errors.Is(err, context.DeadlineExceeded):
		writeV1PlaybackError(w, http.StatusGatewayTimeout, V1ErrorInternal,
			"The server could not authorize the subtitle session in time.", nil)
	case errors.Is(err, context.Canceled):
		writeV1PlaybackError(w, http.StatusRequestTimeout, V1ErrorInvalidRequest,
			"The subtitle request was canceled.", nil)
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
	case models.PlaybackTransportDirectDASH:
		return s.Cfg.Client.DirectDASHEnabled
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
	ctx, cancel := context.WithTimeout(
		context.Background(), v1PlaybackCleanupTimeout)
	defer cancel()
	for _, item := range []struct {
		enabled   bool
		transport string
	}{
		{s.Cfg.Client.DirectHTTPEnabled, models.PlaybackTransportDirectHTTP},
		{s.Cfg.Client.DirectDASHEnabled, models.PlaybackTransportDirectDASH},
		{s.Cfg.Client.ServerProxyV1Enabled, models.PlaybackTransportServerProxy},
		{s.Cfg.Client.ServerHLSV1Enabled, models.PlaybackTransportServerHLS},
	} {
		if !item.enabled {
			revokeV1PlaybackSubtitleSessionsForTransportContext(
				ctx, s.Store, item.transport)
		}
	}
}

type v1PlaybackSubtitleSessionContextRevoker interface {
	RevokePlaybackSubtitleSessionsForTransportContext(
		context.Context, string, time.Time) (int64, error)
	RevokeAllPlaybackSubtitleSessionsContext(
		context.Context, time.Time) (int64, error)
}

func (s *Server) revokeV1PlaybackSubtitleSessionsForTransport(transport string) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(
		context.Background(), v1PlaybackCleanupTimeout)
	defer cancel()
	revokeV1PlaybackSubtitleSessionsForTransportContext(ctx, s.Store, transport)
}

func revokeV1PlaybackSubtitleSessionsForTransport(store any, transport string) {
	ctx, cancel := context.WithTimeout(
		context.Background(), v1PlaybackCleanupTimeout)
	defer cancel()
	revokeV1PlaybackSubtitleSessionsForTransportContext(ctx, store, transport)
}

func revokeV1PlaybackSubtitleSessionsForTransportContext(
	ctx context.Context,
	store any,
	transport string,
) {
	revoker, ok := store.(v1PlaybackSubtitleSessionContextRevoker)
	if !ok {
		return
	}
	_, err := revoker.RevokePlaybackSubtitleSessionsForTransportContext(
		ctx, transport, time.Now().UTC())
	if err != nil {
		// Errors can contain storage connection details, never log them.
		log.Printf("playback subtitle session cleanup failed transport=%s", transport)
	}
}

func (s *Server) revokeAllV1PlaybackSubtitleSessions() {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(
		context.Background(), v1PlaybackCleanupTimeout)
	defer cancel()
	revokeAllV1PlaybackSubtitleSessionsContext(ctx, s.Store)
}

func revokeAllV1PlaybackSubtitleSessionsFromStore(store any) {
	ctx, cancel := context.WithTimeout(
		context.Background(), v1PlaybackCleanupTimeout)
	defer cancel()
	revokeAllV1PlaybackSubtitleSessionsContext(ctx, store)
}

func revokeAllV1PlaybackSubtitleSessionsContext(ctx context.Context, store any) {
	revoker, ok := store.(v1PlaybackSubtitleSessionContextRevoker)
	if !ok {
		return
	}
	_, err := revoker.RevokeAllPlaybackSubtitleSessionsContext(
		ctx, time.Now().UTC())
	if err != nil {
		// Fail closed on the current request; keep DB details out of logs.
		log.Printf("playback subtitle session global cleanup failed")
	}
}
