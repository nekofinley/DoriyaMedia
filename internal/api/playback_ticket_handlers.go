package api

import (
	"bufio"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
	v1PlaybackContractVersion  = 1
	v1PlaybackProbeTimeout     = 150 * time.Second
	v1PlaybackCreateProbeTTL   = 20 * time.Second
	v1PlaybackFinalizeTimeout  = 20 * time.Second
	v1PlaybackIssuanceTimeout  = 20 * time.Second
	v1PlaybackDirectURLTimeout = 5 * time.Second
	v1PlaybackCleanupTimeout   = 3 * time.Second
)

var (
	v1PlaybackCodecPattern            = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]*$`)
	v1PlaybackTokenPattern            = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]*$`)
	errV1PlaybackCandidateUnavailable = errors.New("playback transport candidate unavailable")
)

type v1PlaybackClientCapabilities struct {
	Platform        string   `json:"platform"`
	Version         string   `json:"version"`
	Engine          string   `json:"engine"`
	Containers      []string `json:"containers"`
	VideoCodecs     []string `json:"video_codecs"`
	AudioCodecs     []string `json:"audio_codecs"`
	SubtitleCodecs  []string `json:"subtitle_codecs"`
	SupportsRange   bool     `json:"supports_range"`
	SupportsHeaders bool     `json:"supports_headers"`
}

type v1PlaybackTicketCreateRequest struct {
	FileID              int64                        `json:"file_id"`
	DeviceID            string                       `json:"device_id"`
	Client              v1PlaybackClientCapabilities `json:"client"`
	PreferredTransports []string                     `json:"preferred_transports"`
}

type v1PlaybackTicketResponse struct {
	TicketID            string                    `json:"ticket_id"`
	State               string                    `json:"state"`
	ContractVersion     int                       `json:"contract_version"`
	ExpiresAt           time.Time                 `json:"expires_at"`
	Transport           *v1PlaybackTransport      `json:"transport,omitempty"`
	Media               *media.NativeMediaSummary `json:"media,omitempty"`
	RemainingTransports []string                  `json:"remaining_transports"`
	Message             string                    `json:"message,omitempty"`
	FailureCode         string                    `json:"failure_code,omitempty"`
}

type v1PlaybackTransport struct {
	Kind                 string                  `json:"kind"`
	SourceExpiresAt      *time.Time              `json:"source_expires_at,omitempty"`
	MediaSession         *v1PlaybackMediaSession `json:"media_session,omitempty"`
	Streams              []v1PlaybackStream      `json:"streams"`
	Subtitles            []v1PlaybackSubtitle    `json:"subtitles,omitempty"`
	requestFallbackCount int
	ticketAttemptOrdinal int
	lastClaimedTransport string
}

type v1PlaybackMediaSession struct {
	IdleExpiresAt     time.Time `json:"idle_expires_at"`
	AbsoluteExpiresAt time.Time `json:"absolute_expires_at"`
}

type v1PlaybackStream struct {
	Role    string               `json:"role"`
	URL     string               `json:"url"`
	MIME    string               `json:"mime"`
	Headers source.StreamHeaders `json:"headers"`
	Range   bool                 `json:"range"`
}

type v1PlaybackSubtitle struct {
	ID       string `json:"id"`
	Language string `json:"language"`
	Label    string `json:"label"`
	Format   string `json:"format"`
	URL      string `json:"url,omitempty"`
	Default  bool   `json:"default"`
	Forced   bool   `json:"forced"`
}

func (s *Server) requireV1PlaybackFeature(
	w http.ResponseWriter,
	r *http.Request,
	access *v1DeviceAccessContext,
) bool {
	if access == nil {
		writeV1PlaybackError(w, http.StatusForbidden, V1ErrorFeatureDisabled,
			"The playback ticket feature is disabled.", nil)
		return false
	}
	features := s.effectiveClientFeatures(access.Overrides)
	if !features.V1Enabled || !features.PlaybackTicketEnabled {
		writeV1PlaybackError(w, http.StatusNotFound, V1ErrorFeatureDisabled,
			"The playback ticket feature is disabled.", nil)
		return false
	}
	return true
}

func (s *Server) handleV1PlaybackTicketCreate(
	w http.ResponseWriter,
	r *http.Request,
	access *v1DeviceAccessContext,
) {
	if !s.requireV1PlaybackFeature(w, r, access) {
		return
	}
	var req v1PlaybackTicketCreateRequest
	if err := decodeV1JSON(w, r, &req); err != nil {
		writeV1PlaybackError(w, http.StatusBadRequest, V1ErrorInvalidRequest,
			"The playback ticket request is invalid.", nil)
		return
	}
	client, preferred, clientJSON, err := normalizeV1PlaybackRequest(req)
	if err != nil {
		writeV1PlaybackError(w, http.StatusBadRequest, V1ErrorInvalidRequest, err.Error(), nil)
		return
	}
	if access == nil || access.User == nil || access.Device == nil {
		writeV1PlaybackError(w, http.StatusUnauthorized, V1ErrorUnauthenticated,
			"A device access token is required.", nil)
		return
	}
	if req.DeviceID != access.Device.PublicID {
		writeV1PlaybackError(w, http.StatusForbidden, V1ErrorForbidden,
			"The device does not match the authenticated device.", nil)
		return
	}
	features := s.effectiveClientFeatures(access.Overrides)

	file, drv, err := s.authorizeV1PlaybackFile(access.User, access.Device, req.FileID)
	if err != nil {
		s.writeV1PlaybackFileError(w, err)
		return
	}
	now := time.Now().UTC()
	ticket, err := s.Store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID:              access.User.ID,
		DeviceID:            access.Device.ID,
		FileID:              file.ID,
		Purpose:             models.PlaybackPurposePlayback,
		CandidateTransports: []string{},
		MediaSummary:        json.RawMessage(`{}`),
		ClientCapabilities:  clientJSON,
	}, now)
	if err != nil {
		writeV1PlaybackStoreError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/playback/tickets/"+ticket.TicketID)
	w.Header().Set("Cache-Control", "no-store")
	binding := playbackBinding(ticket)

	ctx, cancel := context.WithTimeout(r.Context(), v1PlaybackCreateProbeTTL)
	defer cancel()
	probe, probeErr := s.Engine.ProbeFor(ctx, file, drv)
	if probeErr != nil {
		failed, finishErr := s.finishV1PlaybackProbeFailed(
			binding, ticket.Version, "source_unavailable")
		if finishErr != nil {
			writeV1PlaybackStoreError(w, finishErr)
			return
		}
		s.writeV1PlaybackTicketResponse(w, http.StatusCreated, failed, features)
		return
	}
	if probe == nil {
		s.Engine.StartProbe(file, drv)
		go s.completeV1PlaybackProbe(
			binding, ticket.Version, file.ID, client, preferred, features)
		s.writeV1PlaybackTicketResponse(w, http.StatusAccepted, ticket, features)
		return
	}

	finalCtx, cancelFinal := s.finalizeV1PlaybackProbeContext()
	defer cancelFinal()
	ticket, err = s.finalizeV1PlaybackTicketCreate(
		finalCtx, binding, ticket, file, drv, probe, client, features)
	if err != nil {
		writeV1PlaybackStoreError(w, err)
		return
	}
	s.writeV1PlaybackCreateResult(w, r, ticket, file, drv, probe, client, features)
}

func (s *Server) finalizeV1PlaybackTicketCreate(
	ctx context.Context,
	binding models.PlaybackTicketBinding,
	ticket *models.PlaybackTicket,
	file *models.MediaFile,
	drv source.Driver,
	probe *media.Probe,
	client media.NativeClientCapabilities,
	features config.ClientFeatures,
) (*models.PlaybackTicket, error) {
	if ticket == nil {
		return nil, db.ErrPlaybackTicketConflict
	}
	version := ticket.Version
	finalized, err := s.finishV1PlaybackTicketWithProbe(
		ctx, binding, version, file, drv, probe, client, features)
	if err == nil {
		if finalized == nil {
			return nil, db.ErrPlaybackTicketConflict
		}
		return finalized, nil
	}
	failed, finishErr := s.finishV1PlaybackProbeFailed(
		binding, version, "source_unavailable")
	if finishErr == nil {
		if failed == nil {
			return nil, db.ErrPlaybackTicketConflict
		}
		return failed, nil
	}
	return nil, err
}

func (s *Server) handleV1PlaybackTicketStatus(
	w http.ResponseWriter,
	r *http.Request,
	access *v1DeviceAccessContext,
) {
	if !s.requireV1PlaybackFeature(w, r, access) {
		return
	}
	ticket, err := s.lookupV1PlaybackTicket(access, r.PathValue("id"))
	if err != nil {
		writeV1PlaybackStoreError(w, err)
		return
	}
	features := s.effectiveClientFeatures(access.Overrides)
	if playbackTicketRequiresNewTicket(ticket, features) {
		writeV1PlaybackTicketTerminalError(
			w, models.PlaybackTicketExhausted, ticket.TicketID)
		return
	}
	s.writeV1PlaybackTicketResponse(w, http.StatusOK, ticket, features)
}

func (s *Server) handleV1PlaybackTicketRefresh(
	w http.ResponseWriter,
	r *http.Request,
	access *v1DeviceAccessContext,
) {
	if !s.requireV1PlaybackFeature(w, r, access) {
		return
	}
	if r.ContentLength != 0 {
		writeV1PlaybackError(w, http.StatusBadRequest, V1ErrorInvalidRequest,
			"The refresh operation does not accept a request body.", nil)
		return
	}
	ticket, err := s.lookupV1PlaybackTicket(access, r.PathValue("id"))
	if err != nil {
		writeV1PlaybackStoreError(w, err)
		return
	}
	if writeV1PlaybackTicketRefreshStateError(w, ticket) {
		return
	}
	binding := playbackBinding(ticket)
	client, _, _, err := decodeStoredPlaybackCapabilities(ticket.ClientCapabilities)
	if err != nil {
		writeV1PlaybackError(w, http.StatusInternalServerError, V1ErrorInternal,
			"The playback ticket state is invalid.", nil)
		return
	}
	file, drv, err := s.authorizeV1PlaybackFile(access.User, access.Device, ticket.FileID)
	if err != nil {
		s.writeV1PlaybackFileError(w, err)
		return
	}
	features := s.effectiveClientFeatures(access.Overrides)
	selection, err := s.playbackTicketTransportSelection(ticket, client, features)
	if err != nil {
		writeV1PlaybackError(w, http.StatusInternalServerError, V1ErrorInternal,
			"The playback ticket state is invalid.", nil)
		return
	}

	issued, transport, err := s.claimV1PlaybackTransport(r, binding, ticket, file, drv, selection)
	if err != nil {
		if errors.Is(err, db.ErrPlaybackTicketExhausted) {
			state := models.PlaybackTicketExhausted
			if issued != nil {
				state = issued.State
			}
			writeV1PlaybackTicketTerminalError(w, state, binding.TicketID)
			return
		}
		writeV1PlaybackStoreError(w, err)
		return
	}
	s.writeV1PlaybackIssuance(w, http.StatusOK, issued, transport, features)
}

func writeV1PlaybackTicketRefreshStateError(
	w http.ResponseWriter,
	ticket *models.PlaybackTicket,
) bool {
	if ticket == nil {
		writeV1PlaybackError(w, http.StatusInternalServerError, V1ErrorInternal,
			"The playback ticket state is invalid.", nil)
		return true
	}
	switch ticket.State {
	case models.PlaybackTicketReady, models.PlaybackTicketConsuming:
		return false
	case models.PlaybackTicketProbing:
		writeV1PlaybackError(w, http.StatusConflict, V1ErrorInvalidRequest,
			"The playback ticket is still being prepared; poll its status before retrying refresh.",
			map[string]any{"action": "status", "state": ticket.State})
		return true
	case models.PlaybackTicketExpired,
		models.PlaybackTicketUnsupported,
		models.PlaybackTicketFailed,
		models.PlaybackTicketExhausted,
		models.PlaybackTicketRevoked:
		writeV1PlaybackTicketTerminalError(w, ticket.State, ticket.TicketID)
		return true
	default:
		writeV1PlaybackError(w, http.StatusInternalServerError, V1ErrorInternal,
			"The playback ticket state is invalid.", nil)
		return true
	}
}

func (s *Server) completeV1PlaybackProbe(
	binding models.PlaybackTicketBinding,
	version int64,
	fileID int64,
	client media.NativeClientCapabilities,
	preferred []media.NativeTransportKind,
	features config.ClientFeatures,
) {
	ctx, cancel := context.WithTimeout(context.Background(), v1PlaybackProbeTimeout)
	defer cancel()
	if err := s.verifyV1PlaybackProbeActive(ctx, binding, version); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			_, _ = s.finishV1PlaybackProbeFailed(
				binding, version, "source_unavailable")
		}
		return
	}
	file, drv, err := s.v1PlaybackFileAndDriver(fileID)
	if err != nil {
		_, _ = s.finishV1PlaybackProbeFailed(
			binding, version, "source_unavailable")
		return
	}
	if err := s.verifyV1PlaybackProbeActive(ctx, binding, version); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			_, _ = s.finishV1PlaybackProbeFailed(
				binding, version, "source_unavailable")
		}
		return
	}
	probe, err := s.Engine.EnsureProbe(ctx, file, drv)
	if err != nil || probe == nil {
		_, _ = s.finishV1PlaybackProbeFailed(
			binding, version, "source_unavailable")
		return
	}
	finalCtx, cancelFinal := s.finalizeV1PlaybackProbeContext()
	defer cancelFinal()
	if _, err := s.finishV1PlaybackTicketWithProbe(
		finalCtx, binding, version, file, drv, probe, client, features); err != nil {
		_, _ = s.finishV1PlaybackProbeFailed(
			binding, version, "source_unavailable")
	}
}

func (s *Server) finalizeV1PlaybackProbeContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), v1PlaybackFinalizeTimeout)
}

func (s *Server) verifyV1PlaybackProbeActive(
	ctx context.Context,
	binding models.PlaybackTicketBinding,
	version int64,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.Store == nil {
		return db.ErrPlaybackTicketNotFound
	}
	return s.Store.CheckPlaybackTicketProbeActive(
		ctx, binding, version, time.Now().UTC())
}

func (s *Server) finishV1PlaybackTicketWithProbe(
	ctx context.Context,
	binding models.PlaybackTicketBinding,
	version int64,
	file *models.MediaFile,
	drv source.Driver,
	probe *media.Probe,
	client media.NativeClientCapabilities,
	features config.ClientFeatures,
) (*models.PlaybackTicket, error) {
	// Embedding discovery depends on this exact probe rather than a stale
	// cached copy on MediaFile. External and driver-published tracks are then
	// registered by the shared discovery path before the immutable id snapshot
	// is written to the ticket.
	if err := s.verifyV1PlaybackProbeActive(ctx, binding, version); err != nil {
		return nil, err
	}
	s.Engine.RegisterEmbeddedSubs(file, probe)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	subtitleIDs := s.snapshotV1PlaybackSubtitleIDs(ctx, file, drv)
	selection, err := s.selectV1PlaybackTransports(
		ctx, file, drv, probe, client, features)
	if err != nil {
		return nil, err
	}
	summaryJSON, err := json.Marshal(selection.Media)
	if err != nil {
		return nil, err
	}
	result := models.PlaybackTicketProbeResult{
		MediaSummary: summaryJSON,
		SubtitleIDs:  subtitleIDs,
	}
	if video := probe.Video(); video != nil {
		index := video.Index
		result.VideoIndex = &index
	}
	if len(selection.Candidates) == 1 &&
		selection.Candidates[0].Kind == media.NativeTransportUnsupported {
		result.State = models.PlaybackTicketUnsupported
		result.FailureCode = "unsupported_media"
	} else {
		result.State = models.PlaybackTicketReady
		result.CandidateTransports = nativeTransportStrings(selection.Kinds())
	}
	if err := s.verifyV1PlaybackProbeActive(ctx, binding, version); err != nil {
		return nil, err
	}
	return s.Store.FinishPlaybackTicketProbeContext(
		ctx, binding, version, result, time.Now().UTC())
}

func (s *Server) finishV1PlaybackProbeFailed(
	binding models.PlaybackTicketBinding,
	version int64,
	code string,
) (*models.PlaybackTicket, error) {
	ctx, cancel := context.WithTimeout(context.Background(), v1PlaybackCleanupTimeout)
	defer cancel()
	if err := s.verifyV1PlaybackProbeActive(ctx, binding, version); err != nil {
		return nil, err
	}
	return s.Store.FinishPlaybackTicketProbeContext(
		ctx,
		binding,
		version,
		models.PlaybackTicketProbeResult{
			State:        models.PlaybackTicketFailed,
			FailureCode:  code,
			MediaSummary: json.RawMessage(`{}`),
		},
		time.Now().UTC(),
	)
}

func (s *Server) writeV1PlaybackCreateResult(
	w http.ResponseWriter,
	r *http.Request,
	ticket *models.PlaybackTicket,
	file *models.MediaFile,
	drv source.Driver,
	probe *media.Probe,
	client media.NativeClientCapabilities,
	features config.ClientFeatures,
) {
	if ticket.State != models.PlaybackTicketReady &&
		ticket.State != models.PlaybackTicketConsuming {
		s.writeV1PlaybackTicketResponse(w, http.StatusCreated, ticket, features)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), v1PlaybackIssuanceTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	selection, err := s.selectV1PlaybackTransports(
		ctx, file, drv, probe, client, features)
	if err != nil {
		writeV1PlaybackStoreError(w, err)
		return
	}
	binding := playbackBinding(ticket)
	issued, transport, err := s.claimV1PlaybackTransport(
		r, binding, ticket, file, drv, selection)
	if err != nil {
		if errors.Is(err, db.ErrPlaybackTicketExhausted) && issued != nil {
			s.writeV1PlaybackTicketResponse(w, http.StatusCreated, issued, features)
			return
		}
		writeV1PlaybackStoreError(w, err)
		return
	}
	s.writeV1PlaybackIssuance(w, http.StatusCreated, issued, transport, features)
}

func (s *Server) claimV1PlaybackTransport(
	r *http.Request,
	binding models.PlaybackTicketBinding,
	ticket *models.PlaybackTicket,
	file *models.MediaFile,
	drv source.Driver,
	selection media.NativeTransportSelection,
) (*models.PlaybackTicket, *v1PlaybackTransport, error) {
	ctx, cancel := context.WithTimeout(r.Context(), v1PlaybackIssuanceTimeout)
	defer cancel()
	r = r.WithContext(ctx)

	current := ticket
	requestFallbackCount := 0
	ticketAttemptOrdinal := 0
	lastClaimedTransport := ""
	if ticket != nil {
		ticketAttemptOrdinal = len(ticket.AttemptedTransports)
		lastClaimedTransport = ticket.SelectedTransport
	}
	for attempts := 0; attempts <= len(current.CandidateTransports); attempts++ {
		if err := ctx.Err(); err != nil {
			return current, nil, err
		}
		claimed, err := s.Store.ClaimNextPlaybackTransport(
			ctx, binding, current.Version, time.Now().UTC())
		if err != nil {
			if claimed != nil && claimed.ClaimToken != "" {
				if releaseErr := s.cleanupV1PlaybackTransportClaim(claimed); releaseErr != nil {
					log.Printf("playback transport claim cleanup failed transport=%s",
						claimed.SelectedTransport)
				}
			}
			return claimed, nil, err
		}
		if err := ctx.Err(); err != nil {
			if releaseErr := s.cleanupV1PlaybackTransportClaim(claimed); releaseErr != nil {
				log.Printf("playback transport claim cleanup failed transport=%s",
					claimed.SelectedTransport)
			}
			return claimed, nil, err
		}
		if !selectionOffersTransport(selection, claimed.SelectedTransport) {
			if err := s.cleanupV1PlaybackTransportClaim(claimed); err != nil {
				return claimed, nil, err
			}
			requestFallbackCount++
			current = playbackTicketAfterClaimRelease(claimed)
			continue
		}
		transport, err := s.issueV1PlaybackTransport(
			r, binding, claimed, file, drv, selection)
		if err != nil {
			releaseErr := s.cleanupV1PlaybackTransportClaim(claimed)
			if errors.Is(err, errV1PlaybackCandidateUnavailable) {
				logV1PlaybackTransportAttemptFailed(
					claimed.SelectedTransport, "source", requestFallbackCount)
				if releaseErr != nil {
					return claimed, nil, releaseErr
				}
				if ctx.Err() != nil {
					return claimed, nil, ctx.Err()
				}
				requestFallbackCount++
				current = playbackTicketAfterClaimRelease(claimed)
				continue
			}
			if releaseErr != nil {
				log.Printf("playback transport claim cleanup failed transport=%s",
					claimed.SelectedTransport)
			}
			return claimed, nil, err
		}
		transport.requestFallbackCount = requestFallbackCount
		transport.ticketAttemptOrdinal = ticketAttemptOrdinal
		transport.lastClaimedTransport = lastClaimedTransport
		return claimed, transport, nil
	}
	return current, nil, db.ErrPlaybackTicketConflict
}

func playbackTicketAfterClaimRelease(ticket *models.PlaybackTicket) *models.PlaybackTicket {
	if ticket == nil {
		return nil
	}
	current := *ticket
	current.ClaimToken = ""
	current.ClaimExpiresAt = nil
	current.Version++
	return &current
}

func (s *Server) releaseV1PlaybackTransportClaim(
	ctx context.Context,
	ticket *models.PlaybackTicket,
) error {
	if ticket == nil {
		return db.ErrPlaybackTicketConflict
	}
	return s.Store.ReleasePlaybackTransportClaim(
		ctx, playbackBinding(ticket), ticket.Version, ticket.SelectedTransport,
		ticket.ClaimToken, time.Now().UTC())
}

func (s *Server) cleanupV1PlaybackTransportClaim(
	ticket *models.PlaybackTicket,
) error {
	ctx, cancel := context.WithTimeout(
		context.Background(), v1PlaybackCleanupTimeout)
	defer cancel()
	return s.releaseV1PlaybackTransportClaim(ctx, ticket)
}

func (s *Server) issueV1PlaybackTransport(
	r *http.Request,
	binding models.PlaybackTicketBinding,
	ticket *models.PlaybackTicket,
	file *models.MediaFile,
	drv source.Driver,
	selection media.NativeTransportSelection,
) (*v1PlaybackTransport, error) {
	ctx := r.Context()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	summary, err := playbackMediaSummary(ticket.MediaSummary)
	if err != nil {
		return nil, err
	}
	switch ticket.SelectedTransport {
	case models.PlaybackTransportDirectHTTP:
		target, headers, err := directURLForPlayback(ctx, drv, file.Path)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err != nil {
			return nil, fmt.Errorf("%w: direct source is unavailable", errV1PlaybackCandidateUnavailable)
		}
		if !v1PlaybackDirectURLAllowed(target) {
			return nil, fmt.Errorf("%w: direct source URL is not safe", errV1PlaybackCandidateUnavailable)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		safeHeaders, err := source.NewStreamHeadersForURL(target, headers)
		if err != nil {
			return nil, fmt.Errorf("%w: direct source headers are not safe", errV1PlaybackCandidateUnavailable)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		transport := &v1PlaybackTransport{
			Kind: models.PlaybackTransportDirectHTTP,
			Streams: []v1PlaybackStream{{
				Role:    "muxed",
				URL:     target,
				MIME:    playbackMIME(summary.Container),
				Headers: safeHeaders,
				Range:   selectionRange(selection, ticket.SelectedTransport),
			}},
		}
		issued := false
		defer func() {
			if !issued {
				s.cleanupV1PlaybackSubtitleIssuance(ticket, transport.Kind)
			}
		}()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if baseURL, err := s.v1PlaybackBaseURL(r); err == nil {
			transport.Subtitles, err = s.issueV1PlaybackSubtitles(
				ticket, file, transport.Kind, "", 0, baseURL)
			if err != nil {
				return nil, err
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := s.Store.ValidatePlaybackTransportIssuance(
			ctx, binding, ticket.Version, ticket.SelectedTransport,
			ticket.ClaimToken, time.Now().UTC()); err != nil {
			return nil, err
		}
		issued = true
		return transport, nil
	case models.PlaybackTransportServerProxy, models.PlaybackTransportServerHLS:
		if ticket.SelectedTransport == models.PlaybackTransportServerHLS &&
			ticket.VideoIndex == models.PlaybackVideoIndexUnknown {
			return nil, fmt.Errorf(
				"%w: media video stream was not frozen by the ticket probe",
				errV1PlaybackCandidateUnavailable)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		baseURL, err := s.v1PlaybackBaseURL(r)
		if err != nil {
			return nil, err
		}
		mode := "proxy"
		if ticket.SelectedTransport == models.PlaybackTransportServerHLS {
			mode = v1PlaybackHLSMode(summary)
		}
		audioOrdinal := -1
		if len(summary.Audio) > 0 {
			audioOrdinal = 0
		}
		grant, err := s.Store.CreatePlaybackMediaSession(
			ctx,
			models.PlaybackMediaSessionRequest{
				Ticket:       binding,
				Transport:    ticket.SelectedTransport,
				Mode:         mode,
				AudioOrdinal: audioOrdinal,
			},
			time.Now().UTC(),
		)
		if err != nil {
			if grant != nil {
				s.cleanupV1PlaybackMediaSessionGrant(
					binding, ticket.SelectedTransport, grant)
				s.cleanupV1PlaybackSubtitleIssuance(
					ticket, ticket.SelectedTransport)
			}
			return nil, err
		}
		issued := false
		defer func() {
			if issued {
				return
			}
			s.cleanupV1PlaybackMediaSessionGrant(
				binding, ticket.SelectedTransport, grant)
			s.cleanupV1PlaybackSubtitleIssuance(ticket, ticket.SelectedTransport)
		}()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		asset := "stream"
		mimeType := playbackMIME(summary.Container)
		if ticket.SelectedTransport == models.PlaybackTransportServerHLS {
			asset = "index.m3u8"
			mimeType = "application/vnd.apple.mpegurl"
		}
		transport := &v1PlaybackTransport{
			Kind: ticket.SelectedTransport,
			MediaSession: &v1PlaybackMediaSession{
				IdleExpiresAt:     grant.Session.IdleExpiresAt,
				AbsoluteExpiresAt: grant.Session.AbsoluteExpiresAt,
			},
			Streams: []v1PlaybackStream{{
				Role:    "muxed",
				URL:     baseURL + "/api/v1/playback/media-sessions/" + grant.Capability + "/" + asset,
				MIME:    mimeType,
				Headers: source.StreamHeaders{},
				Range: ticket.SelectedTransport == models.PlaybackTransportServerProxy &&
					selectionRange(selection, ticket.SelectedTransport),
			}},
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		transport.Subtitles, err = s.issueV1PlaybackSubtitles(
			ticket, file, transport.Kind, grant.Capability,
			grant.Session.StartSeconds, baseURL)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := s.Store.ValidatePlaybackTransportIssuance(
			ctx, binding, ticket.Version, ticket.SelectedTransport,
			ticket.ClaimToken, time.Now().UTC()); err != nil {
			return nil, err
		}
		issued = true
		return transport, nil
	default:
		return nil, db.ErrInvalidPlaybackTransport
	}
}

func (s *Server) cleanupV1PlaybackMediaSessionGrant(
	binding models.PlaybackTicketBinding,
	transport string,
	grant *models.PlaybackMediaSessionGrant,
) {
	if s == nil || s.Store == nil || grant == nil || grant.Session == nil {
		return
	}
	if _, err := s.Store.RevokePlaybackMediaSession(
		models.PlaybackMediaSessionBinding{
			Ticket: binding, Transport: transport,
		},
		grant.Capability, grant.Session.Version, time.Now().UTC(),
	); err != nil {
		log.Printf("playback transport media cleanup failed transport=%s", transport)
	}
}

func (s *Server) handleV1PlaybackMediaStream(w http.ResponseWriter, r *http.Request) {
	telemetry := newV1PlaybackTelemetry(w, models.PlaybackTransportServerProxy, r)
	defer telemetry.finish()
	w = telemetry
	// GET routes also serve HEAD in net/http. Suppress the JSON error body while
	// retaining the shared status and headers for header-only clients.
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
	if !s.Cfg.Client.ServerProxyV1Enabled {
		s.revokeV1PlaybackMediaSessionsForTransport(models.PlaybackTransportServerProxy)
		writeV1PlaybackError(w, http.StatusForbidden, V1ErrorTransportDisabled,
			"The server-proxy transport is disabled.", nil)
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeV1PlaybackError(w, http.StatusBadRequest, V1ErrorInvalidRequest,
			"Media-session URLs do not accept query parameters.", nil)
		return
	}
	capability, ok := v1PlaybackCapability(r.PathValue("id"))
	if !ok {
		writeV1PlaybackError(w, http.StatusNotFound, V1ErrorMediaSessionNotFound,
			"The media session was not found.", nil)
		return
	}
	session, ok := s.authorizeV1PlaybackMediaSession(w, r, capability, models.PlaybackTransportServerProxy)
	if !ok {
		return
	}
	file, drv, err := s.v1PlaybackFileAndDriver(session.Session.FileID)
	if err != nil {
		writeV1PlaybackError(w, http.StatusBadGateway, V1ErrorSourceUnavailable,
			"The media source is unavailable.", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	inputs, err := s.Engine.SegmentInputs(ctx, file, drv, "")
	if err != nil || len(inputs) != 1 {
		writeV1PlaybackError(w, http.StatusBadGateway, V1ErrorSourceUnavailable,
			"The media source is unavailable.", nil)
		return
	}
	input := inputs[0]
	if strings.HasPrefix(input.Target, "http://") ||
		strings.HasPrefix(input.Target, "https://") {
		client, err := v1PlaybackProxyClientForDriver(
			ctx, input.Target, input.Proxy, input.Headers, drv)
		if err != nil {
			writeV1PlaybackError(w, http.StatusBadGateway, V1ErrorSourceUnavailable,
				"The media source is unavailable.", nil)
			return
		}
		defer client.CloseIdleConnections()
		serveV1ProxyMedia(w, r, input.Target, input.Headers, client)
		return
	}
	if local, ok := drv.(*source.LocalDriver); ok {
		opened, err := local.Open(ctx, file.Path)
		if err != nil {
			writeV1PlaybackError(w, http.StatusBadGateway, V1ErrorSourceUnavailable,
				"The media source is unavailable.", nil)
			return
		}
		f, ok := opened.(*os.File)
		if !ok {
			_ = opened.Close()
			writeV1PlaybackError(w, http.StatusBadGateway, V1ErrorSourceUnavailable,
				"The media source is unavailable.", nil)
			return
		}
		defer f.Close()
		serveV1LocalMediaFile(w, r, input.Target, f)
		return
	}
	serveV1LocalMedia(w, r, input.Target)
}

func (s *Server) authorizeV1PlaybackMediaSession(
	w http.ResponseWriter,
	r *http.Request,
	capability string,
	transport string,
) (*db.PlaybackMediaSessionAccess, bool) {
	requireEmailVerification := s.Cfg != nil && s.Cfg.EmailVerificationRequired()
	for attempts := 0; attempts < 4; attempts++ {
		access, err := s.Store.AuthorizePlaybackMediaSessionContext(
			r.Context(), capability, requireEmailVerification, time.Now().UTC())
		if err == nil {
			if access == nil || access.Session == nil ||
				access.Session.Transport != transport ||
				access.Session.State != models.PlaybackMediaSessionActive {
				writeV1PlaybackError(w, http.StatusNotFound, V1ErrorMediaSessionNotFound,
					"The media session was not found.", nil)
				return nil, false
			}
			return access, true
		}
		if errors.Is(err, db.ErrPlaybackMediaSessionConflict) && attempts < 3 {
			select {
			case <-r.Context().Done():
				return nil, false
			case <-time.After(time.Duration(attempts+1) * 5 * time.Millisecond):
			}
			continue
		}
		writeV1PlaybackMediaSessionError(w, err)
		return nil, false
	}
	writeV1PlaybackError(w, http.StatusConflict, V1ErrorInvalidRequest,
		"The media session is busy; retry the request.", map[string]any{"action": "retry"})
	return nil, false
}

type v1PlaybackMediaSessionRevoker interface {
	RevokePlaybackMediaSessionsForTransport(string, time.Time) (int64, error)
}

func (s *Server) revokeV1PlaybackMediaSessionsForTransport(transport string) {
	if s == nil || s.Store == nil {
		return
	}
	v1PlaybackRevokeMediaSessionsForTransport(s.Store, transport)
}

func (s *Server) revokeV1PlaybackAllMediaSessions() {
	if s == nil || s.Store == nil {
		return
	}
	v1PlaybackRevokeMediaSessionsForTransport(
		s.Store, models.PlaybackTransportServerProxy)
	v1PlaybackRevokeMediaSessionsForTransport(
		s.Store, models.PlaybackTransportServerHLS)
	s.revokeAllV1PlaybackSubtitleSessions()
}

func v1PlaybackRevokeMediaSessionsForTransport(
	store v1PlaybackMediaSessionRevoker,
	transport string,
) {
	if store == nil {
		return
	}
	if _, err := store.RevokePlaybackMediaSessionsForTransport(
		transport, time.Now().UTC()); err != nil {
		// The DB error can contain source paths or connection details. Keep the
		// emergency shutdown fail-closed without writing it to logs.
		log.Printf("playback media session cleanup failed transport=%s", transport)
	}
	revokeV1PlaybackSubtitleSessionsForTransport(store, transport)
}

func (s *Server) lookupV1PlaybackTicket(
	access *v1DeviceAccessContext,
	ticketID string,
) (*models.PlaybackTicket, error) {
	if access == nil || access.User == nil || access.Device == nil {
		return nil, db.ErrInvalidPlaybackTicket
	}
	return s.Store.PlaybackTicketByPublicID(
		access.User.ID, access.Device.ID, ticketID, time.Now().UTC())
}

func (s *Server) authorizeV1PlaybackFile(
	user *models.User,
	device *models.Device,
	fileID int64,
) (*models.MediaFile, source.Driver, error) {
	if s == nil || s.Store == nil || s.Engine == nil ||
		user == nil || device == nil || fileID <= 0 ||
		!s.userAllowedForNativeClient(user) || device.RevokedAt != nil {
		return nil, nil, sql.ErrNoRows
	}
	file, err := s.Store.FileByID(fileID)
	if err != nil {
		return nil, nil, err
	}
	drv, err := s.Engine.DriverFor(file)
	if err != nil {
		return nil, nil, err
	}
	return file, drv, nil
}

func (s *Server) v1PlaybackFileAndDriver(fileID int64) (*models.MediaFile, source.Driver, error) {
	if s == nil || s.Store == nil || s.Engine == nil || fileID <= 0 {
		return nil, nil, sql.ErrNoRows
	}
	file, err := s.Store.FileByID(fileID)
	if err != nil {
		return nil, nil, err
	}
	drv, err := s.Engine.DriverFor(file)
	if err != nil {
		return nil, nil, err
	}
	return file, drv, nil
}

func (s *Server) selectV1PlaybackTransports(
	ctx context.Context,
	file *models.MediaFile,
	drv source.Driver,
	probe *media.Probe,
	client media.NativeClientCapabilities,
	features config.ClientFeatures,
) (media.NativeTransportSelection, error) {
	sourceURL := ""
	var sourceHeaders map[string]string
	if drv != nil && drv.HasDirect() {
		target, headers, err := directURLForPlayback(ctx, drv, file.Path)
		if err == nil {
			sourceURL = target
			sourceHeaders = headers
		} else if ctx.Err() != nil {
			return media.NativeTransportSelection{}, ctx.Err()
		} else {
			log.Printf("v1 playback: direct source unavailable for file %d", file.ID)
		}
	}
	_, multiPart := drv.(source.MultiPart)
	return media.SelectNativeTransports(media.NativeTransportInput{
		Probe:         probe,
		SourceURL:     sourceURL,
		SourceHeaders: sourceHeaders,
		MultiPart:     multiPart,
		Client:        client,
		Features: media.NativeTransportFeatureFlags{
			DirectHTTPEnabled:    features.DirectHTTPEnabled,
			ServerProxyV1Enabled: features.ServerProxyV1Enabled,
			ServerHLSV1Enabled:   features.ServerHLSV1Enabled,
		},
	}), nil
}

type v1PlaybackDirectURLResult struct {
	target  string
	headers map[string]string
	err     error
}

// directURLForPlayback bounds callers even when a trusted driver ignores its
// context. A non-cooperative driver can leave its own goroutine behind, but it
// cannot keep a ticket claim or the HTTP handler alive indefinitely.
func directURLForPlayback(
	ctx context.Context,
	drv source.Driver,
	path string,
) (string, map[string]string, error) {
	if drv == nil {
		return "", nil, source.ErrNoDirect
	}
	callCtx, cancel := context.WithTimeout(ctx, v1PlaybackDirectURLTimeout)
	defer cancel()
	result := make(chan v1PlaybackDirectURLResult, 1)
	go func() {
		target, headers, err := drv.DirectURL(callCtx, path)
		result <- v1PlaybackDirectURLResult{
			target: target, headers: headers, err: err,
		}
	}()
	select {
	case <-callCtx.Done():
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		return "", nil, fmt.Errorf(
			"%w: direct source URL timed out", errV1PlaybackCandidateUnavailable)
	case got := <-result:
		return got.target, got.headers, got.err
	}
}

// playbackTicketTransportSelection rebuilds only the client-safe view of the
// immutable candidate plan already stored on the ticket. Refresh must not
// re-probe the source, widen eligibility, or reconstruct an issued transport.
func (s *Server) playbackTicketTransportSelection(
	ticket *models.PlaybackTicket,
	client media.NativeClientCapabilities,
	features config.ClientFeatures,
) (media.NativeTransportSelection, error) {
	summary, err := playbackMediaSummary(ticket.MediaSummary)
	if err != nil {
		return media.NativeTransportSelection{}, err
	}
	selection := media.NativeTransportSelection{
		Media:      summary,
		Candidates: []media.NativeTransportCandidate{},
	}
	for _, raw := range ticket.CandidateTransports {
		kind := media.NativeTransportKind(raw)
		if !v1PlaybackTransportEnabled(features, kind) {
			continue
		}
		selection.Candidates = append(selection.Candidates, media.NativeTransportCandidate{
			Kind:  kind,
			Range: client.SupportsRange,
		})
	}
	return selection, nil
}

func v1PlaybackTransportEnabled(features config.ClientFeatures, kind media.NativeTransportKind) bool {
	switch kind {
	case media.NativeTransportDirectHTTP:
		return features.DirectHTTPEnabled
	case media.NativeTransportServerProxy:
		return features.ServerProxyV1Enabled
	case media.NativeTransportServerHLS:
		return features.ServerHLSV1Enabled
	default:
		return false
	}
}

func (s *Server) writeV1PlaybackTicketResponse(
	w http.ResponseWriter,
	status int,
	ticket *models.PlaybackTicket,
	features config.ClientFeatures,
) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, s.v1PlaybackTicketStatus(ticket, features))
}

func (s *Server) writeV1PlaybackIssuance(
	w http.ResponseWriter,
	status int,
	ticket *models.PlaybackTicket,
	transport *v1PlaybackTransport,
	features config.ClientFeatures,
) {
	response := s.v1PlaybackTicketStatus(ticket, features)
	response.Transport = transport
	w.Header().Set("Cache-Control", "no-store")
	if err := writeJSONResponse(w, status, response); err != nil {
		s.cleanupV1PlaybackWriteFailure(ticket, transport)
		return
	}
	if transport != nil {
		remainingFallbackCount := 0
		if ticket != nil {
			remainingFallbackCount = len(playbackRemainingTransports(ticket, features))
		}
		logV1PlaybackTransportIssued(
			transport.Kind,
			transport.requestFallbackCount,
			remainingFallbackCount,
			transport.ticketAttemptOrdinal,
			transport.lastClaimedTransport,
		)
	}
}

func (s *Server) cleanupV1PlaybackWriteFailure(
	ticket *models.PlaybackTicket,
	transport *v1PlaybackTransport,
) {
	if s == nil || s.Store == nil || ticket == nil {
		return
	}
	binding := playbackBinding(ticket)
	now := time.Now().UTC()
	if transport == nil {
		return
	}
	if _, err := s.Store.RevokePlaybackMediaSessionsForTicketTransport(
		binding, transport.Kind, now); err != nil {
		log.Printf("playback response failure media cleanup failed")
	}
	if _, err := s.Store.RevokePlaybackSubtitleSessionsForTicket(
		binding, transport.Kind, now); err != nil {
		log.Printf("playback response failure subtitle cleanup failed transport=%s",
			transport.Kind)
	}
}

func (s *Server) v1PlaybackTicketStatus(
	ticket *models.PlaybackTicket,
	features config.ClientFeatures,
) v1PlaybackTicketResponse {
	response := v1PlaybackTicketResponse{
		TicketID:            ticket.TicketID,
		State:               ticket.State,
		ContractVersion:     v1PlaybackContractVersion,
		ExpiresAt:           ticket.ExpiresAt,
		RemainingTransports: playbackRemainingTransports(ticket, features),
		FailureCode:         playbackPublicFailureCode(ticket),
	}
	switch ticket.State {
	case models.PlaybackTicketReady,
		models.PlaybackTicketConsuming,
		models.PlaybackTicketExhausted:
		if summary, err := playbackMediaSummary(ticket.MediaSummary); err == nil {
			response.Media = &summary
		}
	}
	switch ticket.State {
	case models.PlaybackTicketUnsupported:
		response.Message = "No supported transport is available for this media."
	case models.PlaybackTicketFailed:
		response.Message = "Playback preparation failed."
	case models.PlaybackTicketExpired:
		response.Message = "This playback ticket has expired."
	case models.PlaybackTicketRevoked:
		response.Message = "This playback ticket has been revoked."
	case models.PlaybackTicketExhausted:
		response.Message = "No untried playback transport remains."
	}
	return response
}

func playbackPublicFailureCode(ticket *models.PlaybackTicket) string {
	if ticket == nil {
		return ""
	}
	switch ticket.State {
	case models.PlaybackTicketUnsupported:
		return "unsupported_media"
	case models.PlaybackTicketFailed:
		return "source_unavailable"
	}
	return ""
}

func playbackRemainingTransports(
	ticket *models.PlaybackTicket,
	features config.ClientFeatures,
) []string {
	remaining := []string{}
	switch ticket.State {
	case models.PlaybackTicketReady:
		for _, transport := range ticket.CandidateTransports {
			if v1PlaybackTransportEnabled(features, media.NativeTransportKind(transport)) {
				remaining = append(remaining, transport)
			}
		}
		return remaining
	case models.PlaybackTicketConsuming:
	default:
		return remaining
	}
	attempted := make(map[string]bool, len(ticket.AttemptedTransports))
	for _, transport := range ticket.AttemptedTransports {
		attempted[transport] = true
	}
	for _, transport := range ticket.CandidateTransports {
		if !attempted[transport] &&
			v1PlaybackTransportEnabled(features, media.NativeTransportKind(transport)) {
			remaining = append(remaining, transport)
		}
	}
	return remaining
}

// playbackTicketRequiresNewTicket prevents a schema-invalid ready response with
// no remaining transports. A consuming ticket may legitimately have an empty
// remaining list after its last candidate, but not when its selected transport
// has been disabled and no enabled fallback remains.
func playbackTicketRequiresNewTicket(
	ticket *models.PlaybackTicket,
	features config.ClientFeatures,
) bool {
	if ticket == nil {
		return false
	}
	remaining := playbackRemainingTransports(ticket, features)
	switch ticket.State {
	case models.PlaybackTicketReady:
		return len(remaining) == 0
	case models.PlaybackTicketConsuming:
		return len(remaining) == 0 &&
			!v1PlaybackTransportEnabled(
				features, media.NativeTransportKind(ticket.SelectedTransport))
	default:
		return false
	}
}

func playbackBinding(ticket *models.PlaybackTicket) models.PlaybackTicketBinding {
	return models.PlaybackTicketBinding{
		UserID:   ticket.UserID,
		DeviceID: ticket.DeviceID,
		FileID:   ticket.FileID,
		TicketID: ticket.TicketID,
	}
}

func playbackMediaSummary(raw json.RawMessage) (media.NativeMediaSummary, error) {
	summary := media.NativeMediaSummary{}
	if len(raw) == 0 {
		return summary, errors.New("empty media summary")
	}
	if err := json.Unmarshal(raw, &summary); err != nil {
		return summary, err
	}
	if summary.Audio == nil {
		summary.Audio = []media.NativeAudioSummary{}
	}
	return summary, nil
}

func nativeTransportStrings(kinds []media.NativeTransportKind) []string {
	out := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		if kind != media.NativeTransportUnsupported {
			out = append(out, string(kind))
		}
	}
	return out
}

func selectionOffersTransport(
	selection media.NativeTransportSelection,
	kind string,
) bool {
	for _, candidate := range selection.Candidates {
		if string(candidate.Kind) == kind {
			return true
		}
	}
	return false
}

func selectionRange(selection media.NativeTransportSelection, kind string) bool {
	for _, candidate := range selection.Candidates {
		if string(candidate.Kind) == kind {
			return candidate.Range
		}
	}
	return false
}

func playbackMIME(container string) string {
	switch strings.ToLower(container) {
	case "mp4":
		return "video/mp4"
	case "mkv":
		return "video/x-matroska"
	case "webm":
		return "video/webm"
	case "mpegts":
		return "video/mp2t"
	default:
		return "application/octet-stream"
	}
}

func v1PlaybackHLSMode(summary media.NativeMediaSummary) string {
	for _, audio := range summary.Audio {
		switch strings.ToLower(audio.Codec) {
		case "aac", "mp3":
		default:
			return "remux-aac"
		}
	}
	return "remux"
}

func v1PlaybackDirectURLAllowed(raw string) bool {
	return media.ValidNativeDirectURL(raw)
}

func (s *Server) v1PlaybackBaseURL(r *http.Request) (string, error) {
	raw := ""
	if s != nil && s.Cfg != nil {
		raw = strings.TrimSpace(s.Cfg.Users.BaseURL)
	}
	if raw == "" {
		if r == nil || s == nil || s.Cfg == nil || s.v1RequestScheme(r) != "https" {
			return "", errors.New("server media transport requires HTTPS")
		}
		raw = "https://" + strings.TrimSpace(r.Host)
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") ||
		u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.EscapedPath() != "" && u.EscapedPath() != "/") {
		return "", errors.New("invalid HTTPS base URL")
	}
	return "https://" + u.Host, nil
}

func normalizeV1PlaybackRequest(
	req v1PlaybackTicketCreateRequest,
) (media.NativeClientCapabilities, []media.NativeTransportKind, json.RawMessage, error) {
	if req.FileID <= 0 {
		return media.NativeClientCapabilities{}, nil, nil, errors.New("file_id must be positive")
	}
	if strings.TrimSpace(req.DeviceID) == "" ||
		strings.TrimSpace(req.DeviceID) != req.DeviceID {
		return media.NativeClientCapabilities{}, nil, nil, errors.New("device_id is invalid")
	}
	client, err := normalizeV1PlaybackClient(req.Client)
	if err != nil {
		return media.NativeClientCapabilities{}, nil, nil, err
	}
	preferred, err := normalizeV1PreferredTransports(req.PreferredTransports)
	if err != nil {
		return media.NativeClientCapabilities{}, nil, nil, err
	}
	client.PreferredTransports = preferred
	raw, err := json.Marshal(client)
	if err != nil {
		return media.NativeClientCapabilities{}, nil, nil, err
	}
	return client, preferred, raw, nil
}

func normalizeV1PlaybackClient(
	client v1PlaybackClientCapabilities,
) (media.NativeClientCapabilities, error) {
	platform, err := normalizeV1Platform(client.Platform)
	if err != nil {
		return media.NativeClientCapabilities{}, errors.New("client.platform must be windows or android")
	}
	version, err := normalizeV1ClientVersion(client.Version)
	if err != nil {
		return media.NativeClientCapabilities{}, errors.New("client.version is invalid")
	}
	engine := strings.ToLower(strings.TrimSpace(client.Engine))
	switch engine {
	case "mpv", "libmpv", "media3":
	default:
		return media.NativeClientCapabilities{}, errors.New("client.engine is invalid")
	}
	containers, err := normalizeV1PlaybackList(
		client.Containers, 1, 32, 32, v1PlaybackTokenPattern)
	if err != nil {
		return media.NativeClientCapabilities{}, fmt.Errorf("client.containers %w", err)
	}
	video, err := normalizeV1PlaybackList(
		client.VideoCodecs, 0, 32, 64, v1PlaybackCodecPattern)
	if err != nil {
		return media.NativeClientCapabilities{}, fmt.Errorf("client.video_codecs %w", err)
	}
	audio, err := normalizeV1PlaybackList(
		client.AudioCodecs, 0, 32, 64, v1PlaybackCodecPattern)
	if err != nil {
		return media.NativeClientCapabilities{}, fmt.Errorf("client.audio_codecs %w", err)
	}
	subtitles, err := normalizeV1PlaybackList(
		client.SubtitleCodecs, 0, 32, 16,
		regexp.MustCompile(`^(text|ass|pgs)$`))
	if err != nil {
		return media.NativeClientCapabilities{}, fmt.Errorf("client.subtitle_codecs %w", err)
	}
	return media.NativeClientCapabilities{
		Platform:        platform,
		Version:         version,
		Engine:          engine,
		Containers:      containers,
		VideoCodecs:     video,
		AudioCodecs:     audio,
		SubtitleCodecs:  subtitles,
		SupportsRange:   client.SupportsRange,
		SupportsHeaders: client.SupportsHeaders,
	}, nil
}

func normalizeV1PlaybackList(
	values []string,
	minLen, maxLen, maxItemLen int,
	pattern *regexp.Regexp,
) ([]string, error) {
	if len(values) < minLen || len(values) > maxLen {
		return nil, fmt.Errorf("must contain %d-%d values", minLen, maxLen)
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, raw := range values {
		value := strings.ToLower(strings.TrimSpace(raw))
		if strings.TrimSpace(raw) != raw || len(value) < 1 || len(value) > maxItemLen ||
			!pattern.MatchString(value) {
			return nil, errors.New("contains an invalid value")
		}
		if seen[value] {
			return nil, errors.New("contains duplicate values")
		}
		seen[value] = true
		out = append(out, value)
	}
	return out, nil
}

func normalizeV1PreferredTransports(values []string) ([]media.NativeTransportKind, error) {
	if len(values) < 1 || len(values) > 3 {
		return nil, errors.New("preferred_transports must contain 1-3 values")
	}
	out := make([]media.NativeTransportKind, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, raw := range values {
		if raw != strings.TrimSpace(raw) || seen[raw] {
			return nil, errors.New("preferred_transports contains an invalid value")
		}
		kind := media.NativeTransportKind(raw)
		switch kind {
		case media.NativeTransportDirectHTTP,
			media.NativeTransportServerProxy,
			media.NativeTransportServerHLS:
		default:
			return nil, errors.New("preferred_transports contains an unsupported transport")
		}
		seen[raw] = true
		out = append(out, kind)
	}
	return out, nil
}

func decodeStoredPlaybackCapabilities(
	raw json.RawMessage,
) (media.NativeClientCapabilities, []media.NativeTransportKind, json.RawMessage, error) {
	var client media.NativeClientCapabilities
	if len(raw) == 0 || json.Unmarshal(raw, &client) != nil {
		return client, nil, nil, errors.New("invalid stored playback capabilities")
	}
	return client, client.PreferredTransports, raw, nil
}

func v1PlaybackCapability(value string) (string, bool) {
	if len(value) < 22 || len(value) > 96 || !strings.HasPrefix(value, "ms_") {
		return "", false
	}
	for _, r := range value[3:] {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return "", false
		}
	}
	return value, true
}

type v1LocalMediaRequest struct {
	wantRange byteRange
	hasRange  bool
	ifRange   string
}

func parseV1LocalMediaRequest(
	w http.ResponseWriter,
	r *http.Request,
) (v1LocalMediaRequest, bool) {
	var parsed v1LocalMediaRequest
	if r == nil {
		writeV1ProxyError(w, nil, http.StatusInternalServerError, V1ErrorInternal,
			v1ProxyInternalErrorMsg)
		return parsed, false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeV1ProxyError(w, r, http.StatusMethodNotAllowed, V1ErrorInvalidRequest,
			"Only GET and HEAD are supported.")
		return parsed, false
	}
	rangeValues := r.Header.Values("Range")
	if len(rangeValues) > 1 {
		writeV1ProxyError(w, r, http.StatusBadRequest, V1ErrorInvalidRequest,
			v1ProxyInvalidRangeMsg)
		return parsed, false
	}
	if len(rangeValues) == 1 {
		parsed.wantRange, parsed.hasRange = parseRequestRange(rangeValues[0])
		if !parsed.hasRange {
			writeV1ProxyError(w, r, http.StatusBadRequest, V1ErrorInvalidRequest,
				v1ProxyInvalidRangeMsg)
			return parsed, false
		}
	}
	if parsed.hasRange {
		ifRangeValues := r.Header.Values("If-Range")
		if len(ifRangeValues) > 1 {
			writeV1ProxyError(w, r, http.StatusBadRequest, V1ErrorInvalidRequest,
				v1ProxyInvalidRangeMsg)
			return parsed, false
		}
		if len(ifRangeValues) == 1 {
			parsed.ifRange = strings.TrimSpace(ifRangeValues[0])
		}
	}
	return parsed, true
}

func serveV1LocalMedia(w http.ResponseWriter, r *http.Request, target string) {
	parsed, ok := parseV1LocalMediaRequest(w, r)
	if !ok {
		return
	}
	f, err := os.Open(target)
	if err != nil {
		writeV1ProxyError(w, r, http.StatusBadGateway, V1ErrorSourceUnavailable,
			v1ProxySourceUnavailableMsg)
		return
	}
	defer f.Close()
	serveV1LocalMediaFileParsed(w, r, target, f, parsed)
}

func serveV1LocalMediaFile(w http.ResponseWriter, r *http.Request, target string, f *os.File) {
	parsed, ok := parseV1LocalMediaRequest(w, r)
	if !ok {
		return
	}
	serveV1LocalMediaFileParsed(w, r, target, f, parsed)
}

func serveV1LocalMediaFileParsed(
	w http.ResponseWriter,
	r *http.Request,
	target string,
	f *os.File,
	parsed v1LocalMediaRequest,
) {
	if f == nil {
		writeV1ProxyError(w, r, http.StatusBadGateway, V1ErrorSourceUnavailable,
			v1ProxySourceUnavailableMsg)
		return
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeV1ProxyError(w, r, http.StatusBadGateway, V1ErrorSourceUnavailable,
			v1ProxySourceUnavailableMsg)
		return
	}
	total := info.Size()
	modTime := info.ModTime().UTC()
	etag := fmt.Sprintf(`"%x-%x"`, total, modTime.UnixNano())
	contentType := v1LocalMediaContentType(target)
	w.Header().Set("Cache-Control", v1ProxyCacheControl)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Last-Modified", modTime.Format(http.TimeFormat))
	w.Header().Set("ETag", etag)

	honorRange := parsed.hasRange
	if parsed.ifRange != "" &&
		!v1LocalIfRangeMatches(parsed.ifRange, etag, modTime) {
		honorRange = false
	}
	if honorRange {
		if parsed.wantRange.start >= total {
			writeV1ProxyRangeUnsatisfiable(w, r, total)
			return
		}
		end := parsed.wantRange.end
		if end < 0 || end >= total {
			end = total - 1
		}
		length := end - parsed.wantRange.start + 1
		w.Header().Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", parsed.wantRange.start, end, total))
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusPartialContent)
		if r.Method == http.MethodHead {
			return
		}
		if _, err := f.Seek(parsed.wantRange.start, io.SeekStart); err != nil {
			log.Printf("v1 local media: seek failed")
			panic(http.ErrAbortHandler)
		}
		if n, err := io.CopyN(w, f, length); err != nil || n != length {
			log.Printf("v1 local media: body copy failed")
			panic(http.ErrAbortHandler)
		}
		return
	}

	w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if n, err := io.Copy(w, f); err != nil || n != total {
		log.Printf("v1 local media: body copy failed")
		panic(http.ErrAbortHandler)
	}
}

func v1LocalIfRangeMatches(raw, etag string, modTime time.Time) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	// If-Range requires a strong entity-tag comparison. Our ETag is strong;
	// weak or alternate validators must fall back to the full representation.
	if strings.HasPrefix(raw, `"`) {
		return raw == etag
	}
	validator, err := http.ParseTime(raw)
	if err != nil {
		return false
	}
	return modTime.Truncate(time.Second).Equal(validator)
}

func v1LocalMediaContentType(target string) string {
	switch strings.ToLower(filepath.Ext(target)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".ts", ".m2ts", ".mts":
		return "video/mp2t"
	case ".mov":
		return "video/quicktime"
	case ".m4a":
		return "audio/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".flac":
		return "audio/flac"
	case ".ogg", ".oga":
		return "audio/ogg"
	case ".opus":
		return "audio/opus"
	default:
		return "application/octet-stream"
	}
}

func v1PlaybackProxyClient(
	ctx context.Context,
	target string,
	proxyRaw string,
	sourceHeaders map[string]string,
) (*http.Client, error) {
	return v1PlaybackProxyClientWithLookup(
		ctx, target, proxyRaw, sourceHeaders, net.DefaultResolver.LookupIPAddr)
}

func v1PlaybackProxyClientWithLookup(
	ctx context.Context,
	target string,
	proxyRaw string,
	sourceHeaders map[string]string,
	lookup v1PlaybackIPLookup,
) (*http.Client, error) {
	return v1PlaybackProxyClientWithPolicy(
		ctx, target, proxyRaw, sourceHeaders, nil, lookup, nil)
}

// v1PlaybackProxyClientForDriver applies the administrator-configured private
// source boundary. A bare URLDriver gets no LAN exception; only the exact
// scheme+host+port of a configured WebDAV/OpenList base may resolve privately.
func v1PlaybackProxyClientForDriver(
	ctx context.Context,
	target string,
	proxyRaw string,
	sourceHeaders map[string]string,
	drv source.Driver,
) (*http.Client, error) {
	return v1PlaybackProxyClientForDriverWithLookup(
		ctx, target, proxyRaw, sourceHeaders, drv, net.DefaultResolver.LookupIPAddr)
}

func v1PlaybackProxyClientForDriverWithLookup(
	ctx context.Context,
	target string,
	proxyRaw string,
	sourceHeaders map[string]string,
	drv source.Driver,
	lookup v1PlaybackIPLookup,
) (*http.Client, error) {
	return v1PlaybackProxyClientWithPolicy(
		ctx, target, proxyRaw, sourceHeaders, drv, lookup, nil)
}

// The dial hook lets tests map fake public and LAN addresses to loopback
// listeners while exercising the real resolution, redirect and dial policy.
func v1PlaybackProxyClientForDriverWithLookupAndDial(
	ctx context.Context,
	target string,
	proxyRaw string,
	sourceHeaders map[string]string,
	drv source.Driver,
	lookup v1PlaybackIPLookup,
	dial v1PlaybackDialFunc,
) (*http.Client, error) {
	return v1PlaybackProxyClientWithPolicy(
		ctx, target, proxyRaw, sourceHeaders, drv, lookup, dial)
}

func v1PlaybackProxyClientWithPolicy(
	ctx context.Context,
	target string,
	proxyRaw string,
	sourceHeaders map[string]string,
	drv source.Driver,
	lookup v1PlaybackIPLookup,
	dial v1PlaybackDialFunc,
) (*http.Client, error) {
	targetURL, err := url.Parse(target)
	if err != nil {
		return nil, errors.New("invalid proxy target")
	}
	initialOrigin, err := v1PlaybackURLOrigin(targetURL)
	if err != nil {
		return nil, errors.New("invalid proxy target")
	}
	_, initialPrivate, initialLoopback, err := v1PlaybackResolveHostScope(
		ctx, targetURL.Hostname(), lookup)
	if err != nil {
		return nil, err
	}
	trustedPrivateOrigin := ""
	if initialPrivate {
		if configured := v1PlaybackTrustedPrivateOrigin(drv); configured != "" &&
			configured == initialOrigin {
			trustedPrivateOrigin = configured
		} else {
			return nil, errors.New("private-network target is not an administrator-configured source")
		}
	}
	headerNames := make([]string, 0, len(sourceHeaders)+3)
	for name := range sourceHeaders {
		headerNames = append(headerNames, name)
	}
	headerNames = append(headerNames, "Authorization", "Cookie", "Proxy-Authorization")

	proxyURL, proxyAddress, err := v1PlaybackSourceProxy(proxyRaw)
	if err != nil {
		return nil, err
	}
	if dial == nil {
		dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
		dial = dialer.DialContext
	}
	transport := &http.Transport{
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	}
	policy := &v1PlaybackDialPolicy{
		initialOrigin:        initialOrigin,
		initialPrivate:       initialPrivate,
		initialLoopback:      initialLoopback,
		trustedPrivateOrigin: trustedPrivateOrigin,
		lookup:               lookup,
		dial:                 dial,
		sourceProxy:          proxyURL,
		sourceProxyAddress:   proxyAddress,
	}
	transport.DialContext = policy.dialContext
	state := &v1PlaybackRedirectState{
		initialOrigin:        initialOrigin,
		initialPrivate:       initialPrivate,
		initialLoopback:      initialLoopback,
		trustedPrivateOrigin: trustedPrivateOrigin,
		lookup:               lookup,
		headerNames:          headerNames,
	}
	return &http.Client{
		Transport:     &v1PlaybackSchemeTransport{base: transport},
		CheckRedirect: state.check,
	}, nil
}

type v1PlaybackSchemeTransport struct {
	base *http.Transport
}

type v1PlaybackSchemeContextKey struct{}

func (t *v1PlaybackSchemeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t == nil || t.base == nil || req == nil || req.URL == nil {
		return nil, errors.New("invalid proxy request")
	}
	ctx := context.WithValue(
		req.Context(), v1PlaybackSchemeContextKey{}, strings.ToLower(req.URL.Scheme))
	return t.base.RoundTrip(req.WithContext(ctx))
}

func (t *v1PlaybackSchemeTransport) CloseIdleConnections() {
	if t != nil && t.base != nil {
		t.base.CloseIdleConnections()
	}
}

type v1PlaybackRedirectState struct {
	initialOrigin        string
	initialPrivate       bool
	initialLoopback      bool
	trustedPrivateOrigin string
	lookup               v1PlaybackIPLookup
	headerNames          []string
}

func (s *v1PlaybackRedirectState) check(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return errors.New("unsafe redirect")
	}
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	if req.URL == nil || req.URL.User != nil ||
		(req.URL.Scheme != "http" && req.URL.Scheme != "https") ||
		req.URL.Hostname() == "" {
		return errors.New("unsafe redirect")
	}
	previous := via[len(via)-1]
	if previous.URL.Scheme == "https" && req.URL.Scheme != "https" {
		return errors.New("HTTPS downgrade redirect rejected")
	}
	targetOrigin, err := v1PlaybackURLOrigin(req.URL)
	if err != nil {
		return errors.New("unsafe redirect")
	}
	_, targetPrivate, targetLoopback, err := v1PlaybackResolveHostScope(
		req.Context(), req.URL.Hostname(), s.lookup)
	if err != nil {
		return err
	}
	if targetOrigin == s.initialOrigin && targetLoopback != s.initialLoopback {
		return errors.New("redirect changed loopback address class")
	}
	if targetPrivate {
		// Revalidate the exact origin, not just the hostname. A LAN source
		// cannot pivot to another port or another private address.
		if s.trustedPrivateOrigin == "" || targetOrigin != s.trustedPrivateOrigin {
			return errors.New("private-network redirect rejected")
		}
	} else if s.initialPrivate && targetOrigin == s.initialOrigin {
		return errors.New("trusted private origin changed address class")
	}
	// net/http rebuilds redirect headers from the first request on every hop.
	// Comparing only with the previous origin lets A -> B -> B reintroduce A's
	// credentials on the second B request. Anchor the decision to the initial
	// origin for the whole chain.
	if targetOrigin != s.initialOrigin {
		for _, name := range s.headerNames {
			req.Header.Del(name)
		}
		// net/http synthesizes Referer on redirects even when the caller did
		// not supply one. It can contain the source query string, so it must
		// never cross an origin boundary by default.
		req.Header.Del("Referer")
	}
	return nil
}

func sameHTTPOrigin(a, b *url.URL) bool {
	aOrigin, aErr := v1PlaybackURLOrigin(a)
	bOrigin, bErr := v1PlaybackURLOrigin(b)
	return aErr == nil && bErr == nil && aOrigin == bOrigin
}

type v1PlaybackIPLookup func(context.Context, string) ([]net.IPAddr, error)
type v1PlaybackDialFunc func(context.Context, string, string) (net.Conn, error)

type v1PlaybackDialPolicy struct {
	initialOrigin        string
	initialPrivate       bool
	initialLoopback      bool
	trustedPrivateOrigin string
	lookup               v1PlaybackIPLookup
	dial                 v1PlaybackDialFunc
	sourceProxy          *url.URL
	sourceProxyAddress   string
}

func (p *v1PlaybackDialPolicy) dialContext(
	ctx context.Context,
	network, address string,
) (net.Conn, error) {
	scheme, _ := ctx.Value(v1PlaybackSchemeContextKey{}).(string)
	if scheme != "http" && scheme != "https" {
		return nil, errors.New("proxy destination has no approved scheme")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	origin, err := v1PlaybackOriginFromParts(scheme, host, port)
	if err != nil {
		return nil, err
	}
	ips, private, loopback, err := v1PlaybackResolveHostScope(ctx, host, p.lookup)
	if err != nil {
		return nil, err
	}
	if origin == p.initialOrigin && loopback != p.initialLoopback {
		return nil, errors.New("trusted source changed loopback address class")
	}
	if private {
		if p.trustedPrivateOrigin == "" || origin != p.trustedPrivateOrigin {
			return nil, errors.New("private-network dial rejected")
		}
	} else if p.initialPrivate && origin == p.initialOrigin {
		return nil, errors.New("trusted private host changed address class")
	}
	var lastErr error
	for _, candidate := range ips {
		target := net.JoinHostPort(candidate.IP.String(), port)
		var conn net.Conn
		var dialErr error
		if p.sourceProxy != nil {
			conn, dialErr = v1PlaybackDialHTTPProxy(
				ctx, p.sourceProxy, p.sourceProxyAddress, target, p.dial)
		} else {
			conn, dialErr = p.dial(ctx, network, target)
		}
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("proxy target has no reachable address")
}

func v1PlaybackSourceProxy(raw string) (*url.URL, string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, "", nil
	}
	proxyURL, err := url.Parse(raw)
	if err != nil || strings.ToLower(proxyURL.Scheme) != "http" ||
		proxyURL.Hostname() == "" {
		return nil, "", errors.New("invalid source proxy")
	}
	host := v1PlaybackCanonicalHost(proxyURL.Hostname())
	port := proxyURL.Port()
	if port == "" {
		return nil, "", errors.New("invalid source proxy")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || !ip.IsLoopback() {
		return nil, "", errors.New("source proxy must be a loopback bridge")
	}
	proxyURL.Scheme = "http"
	return proxyURL, net.JoinHostPort(ip.String(), port), nil
}

func v1PlaybackDialHTTPProxy(
	ctx context.Context,
	proxyURL *url.URL,
	proxyAddress string,
	target string,
	dial v1PlaybackDialFunc,
) (net.Conn, error) {
	conn, err := dial(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, err
	}
	var request strings.Builder
	request.WriteString("CONNECT ")
	request.WriteString(target)
	request.WriteString(" HTTP/1.1\r\nHost: ")
	request.WriteString(target)
	request.WriteString("\r\n")
	if proxyURL != nil && proxyURL.User != nil {
		user := proxyURL.User.Username()
		pass, _ := proxyURL.User.Password()
		token := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		request.WriteString("Proxy-Authorization: Basic ")
		request.WriteString(token)
		request.WriteString("\r\n")
	}
	request.WriteString("\r\n")
	if _, err := io.WriteString(conn, request.String()); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, errors.New("source proxy rejected the target")
	}
	if reader.Buffered() > 0 {
		return &v1PlaybackBufferedConn{Conn: conn, reader: reader}, nil
	}
	return conn, nil
}

type v1PlaybackBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *v1PlaybackBufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func v1PlaybackURLOrigin(u *url.URL) (string, error) {
	if u == nil || u.User != nil {
		return "", errors.New("invalid proxy target")
	}
	return v1PlaybackOriginFromParts(u.Scheme, u.Hostname(), u.Port())
}

func v1PlaybackOriginFromParts(scheme, host, port string) (string, error) {
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if scheme != "http" && scheme != "https" {
		return "", errors.New("invalid proxy target scheme")
	}
	host = v1PlaybackCanonicalHost(host)
	if host == "" || strings.Contains(host, "%") {
		return "", errors.New("invalid proxy target host")
	}
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", errors.New("invalid proxy target port")
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(number)), nil
}

func v1PlaybackTrustedPrivateOrigin(drv source.Driver) string {
	var raw string
	switch d := drv.(type) {
	case *source.WebDAVDriver:
		if d != nil {
			raw = d.Base
		}
	case *source.OpenListDriver:
		if d != nil {
			raw = d.Base
		}
	default:
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil {
		return ""
	}
	origin, err := v1PlaybackURLOrigin(u)
	if err != nil {
		return ""
	}
	return origin
}

func v1PlaybackResolveHost(
	ctx context.Context,
	host string,
	lookup v1PlaybackIPLookup,
) ([]net.IPAddr, bool, error) {
	ips, private, _, err := v1PlaybackResolveHostScope(ctx, host, lookup)
	return ips, private, err
}

func v1PlaybackResolveHostScope(
	ctx context.Context,
	host string,
	lookup v1PlaybackIPLookup,
) ([]net.IPAddr, bool, bool, error) {
	host = v1PlaybackCanonicalHost(host)
	if host == "" {
		return nil, false, false, errors.New("proxy target has no host")
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		switch v1PlaybackIPClass(ip) {
		case v1PlaybackIPForbidden:
			return nil, false, false, errors.New("proxy target address is forbidden")
		case v1PlaybackIPPrivate:
			return []net.IPAddr{{IP: ip}}, true, ip.IsLoopback(), nil
		default:
			return []net.IPAddr{{IP: ip}}, false, false, nil
		}
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return []net.IPAddr{
			{IP: net.ParseIP("127.0.0.1")},
			{IP: net.ParseIP("::1")},
		}, true, true, nil
	}
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	ips, err := lookup(ctx, host)
	if err != nil {
		return nil, false, false, errors.New("proxy target resolution failed")
	}
	if len(ips) == 0 {
		return nil, false, false, errors.New("proxy target has no addresses")
	}
	var hasPrivate, hasPublic, hasLoopback, hasNetworkPrivate bool
	for _, candidate := range ips {
		switch v1PlaybackIPClass(candidate.IP) {
		case v1PlaybackIPForbidden:
			return nil, false, false, errors.New("proxy target address is forbidden")
		case v1PlaybackIPPrivate:
			hasPrivate = true
			if candidate.IP.IsLoopback() {
				hasLoopback = true
			} else {
				hasNetworkPrivate = true
			}
		case v1PlaybackIPPublic:
			hasPublic = true
		default:
			return nil, false, false, errors.New("proxy target resolution failed")
		}
	}
	if hasPrivate && hasPublic {
		return nil, false, false, errors.New("proxy target has mixed public and private addresses")
	}
	if hasLoopback && hasNetworkPrivate {
		return nil, false, false, errors.New("proxy target has mixed loopback and private addresses")
	}
	return ips, hasPrivate, hasLoopback, nil
}

func v1PlaybackCanonicalHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

type v1PlaybackIPClassValue int

const (
	v1PlaybackIPPublic v1PlaybackIPClassValue = iota
	v1PlaybackIPPrivate
	v1PlaybackIPForbidden
)

func v1PlaybackIPClass(ip net.IP) v1PlaybackIPClassValue {
	if v1PlaybackKnownMetadataIP(ip) {
		return v1PlaybackIPForbidden
	}
	if ip == nil {
		return v1PlaybackIPForbidden
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return v1PlaybackIPPrivate
	}
	if embedded := v1PlaybackTransitionIPv4(ip); embedded != nil {
		if v1PlaybackIPClass(embedded) == v1PlaybackIPPublic {
			return v1PlaybackIPPublic
		}
		return v1PlaybackIPForbidden
	}
	if ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || !ip.IsGlobalUnicast() {
		return v1PlaybackIPForbidden
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 0 ||
			(v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127) ||
			(v4[0] == 198 && (v4[1] == 18 || v4[1] == 19)) ||
			(v4[0] == 192 && v4[1] == 0 && v4[2] == 0) {
			return v1PlaybackIPForbidden
		}
	}
	return v1PlaybackIPPublic
}

func v1PlaybackKnownMetadataIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.Equal(net.IPv4(100, 100, 100, 200)) ||
			v4.Equal(net.IPv4(168, 63, 129, 16))
	}
	return ip.Equal(net.ParseIP("fd00:ec2::254"))
}

func v1PlaybackTransitionIPv4(ip net.IP) net.IP {
	if ip == nil || ip.To4() != nil || len(ip) != net.IPv6len {
		return nil
	}
	allZero := true
	for _, value := range ip[:12] {
		if value != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return net.IPv4(ip[12], ip[13], ip[14], ip[15])
	}
	if ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff &&
		ip[3] == 0x9b && ip[4] == 0 && ip[5] == 0 &&
		ip[6] == 0 && ip[7] == 0 && ip[8] == 0 && ip[9] == 0 &&
		ip[10] == 0 && ip[11] == 0 {
		return net.IPv4(ip[12], ip[13], ip[14], ip[15])
	}
	if ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff &&
		ip[3] == 0x9b && ip[4] == 0x00 && ip[5] == 0x01 {
		return net.IPv4(ip[12], ip[13], ip[14], ip[15])
	}
	if ip[0] == 0x20 && ip[1] == 0x02 {
		return net.IPv4(ip[2], ip[3], ip[4], ip[5])
	}
	return nil
}

func v1PlaybackPrivateIP(ip net.IP) bool {
	return v1PlaybackIPClass(ip) != v1PlaybackIPPublic
}

func writeV1PlaybackError(
	w http.ResponseWriter,
	status int,
	code V1ErrorCode,
	message string,
	details map[string]any,
) {
	setPlaybackFailureStage(w, v1PlaybackFailureStageForError(status, code))
	w.Header().Set("Cache-Control", "no-store")
	writeV1Error(w, status, code, message, details)
}

func (s *Server) writeV1PlaybackFileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeV1PlaybackError(w, http.StatusNotFound, V1ErrorInvalidRequest,
			"The requested file is not available.", nil)
	default:
		writeV1PlaybackError(w, http.StatusBadGateway, V1ErrorSourceUnavailable,
			"The media source is unavailable.", nil)
	}
}

func writeV1PlaybackTicketTerminalError(w http.ResponseWriter, state, ticketID string) {
	details := map[string]any{
		"action": "request_new_ticket",
		"state":  state,
	}
	if ticketID != "" {
		details["ticket_id"] = ticketID
	}
	switch state {
	case models.PlaybackTicketRevoked:
		details["action"] = "reauthorize"
		writeV1PlaybackError(w, http.StatusForbidden, V1ErrorForbidden,
			"This playback ticket has been revoked.", details)
	case models.PlaybackTicketUnsupported:
		writeV1PlaybackError(w, http.StatusGone, V1ErrorTicketExpired,
			"This playback ticket has no supported transport.", details)
	case models.PlaybackTicketFailed:
		writeV1PlaybackError(w, http.StatusGone, V1ErrorTicketExpired,
			"This playback ticket failed and cannot be refreshed.", details)
	case models.PlaybackTicketExhausted:
		writeV1PlaybackError(w, http.StatusGone, V1ErrorTicketExpired,
			"This playback ticket has no untried transport.", details)
	case models.PlaybackTicketExpired:
		writeV1PlaybackError(w, http.StatusGone, V1ErrorTicketExpired,
			"This playback ticket has expired.", details)
	default:
		writeV1PlaybackError(w, http.StatusGone, V1ErrorTicketExpired,
			"This playback ticket can no longer be refreshed.", details)
	}
}

func writeV1PlaybackStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		writeV1PlaybackError(w, http.StatusGatewayTimeout, V1ErrorInternal,
			"The server could not complete the playback request in time.",
			map[string]any{"action": "retry"})
	case errors.Is(err, context.Canceled):
		writeV1PlaybackError(w, http.StatusRequestTimeout, V1ErrorInvalidRequest,
			"The playback request was canceled.", nil)
	case errors.Is(err, db.ErrPlaybackTicketNotFound):
		writeV1PlaybackError(w, http.StatusNotFound, V1ErrorTicketNotFound,
			"The playback ticket was not found.",
			map[string]any{"action": "request_new_ticket"})
	case errors.Is(err, db.ErrPlaybackTicketExpired):
		writeV1PlaybackTicketTerminalError(
			w, models.PlaybackTicketExpired, "")
	case errors.Is(err, db.ErrPlaybackTicketRevoked):
		writeV1PlaybackTicketTerminalError(
			w, models.PlaybackTicketRevoked, "")
	case errors.Is(err, db.ErrDevicePlaybackDisabled):
		writeV1DeviceFeatureDisabled(w)
	case errors.Is(err, db.ErrPlaybackTicketExhausted):
		writeV1PlaybackTicketTerminalError(
			w, models.PlaybackTicketExhausted, "")
	case errors.Is(err, db.ErrPlaybackTicketConflict),
		errors.Is(err, db.ErrPlaybackMediaSessionConflict):
		writeV1PlaybackError(w, http.StatusConflict, V1ErrorInvalidRequest,
			"The playback state changed; retry the request.",
			map[string]any{"action": "retry"})
	case errors.Is(err, db.ErrInvalidPlaybackTicket),
		errors.Is(err, db.ErrInvalidPlaybackTransport),
		errors.Is(err, db.ErrInvalidPlaybackMediaSession):
		writeV1PlaybackError(w, http.StatusBadRequest, V1ErrorInvalidRequest,
			"The playback request is invalid.", nil)
	default:
		writeV1PlaybackError(w, http.StatusInternalServerError, V1ErrorInternal,
			"The server could not complete the playback request.", nil)
	}
}

func writeV1PlaybackMediaSessionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrPlaybackMediaSessionNotFound):
		writeV1PlaybackError(w, http.StatusNotFound, V1ErrorMediaSessionNotFound,
			"The media session was not found.", nil)
	case errors.Is(err, db.ErrPlaybackMediaSessionExpired):
		writeV1PlaybackError(w, http.StatusGone, V1ErrorMediaSessionExpired,
			"This media session has expired.", nil)
	case errors.Is(err, db.ErrPlaybackMediaSessionRevoked),
		errors.Is(err, db.ErrDeviceRevoked):
		writeV1PlaybackError(w, http.StatusForbidden, V1ErrorForbidden,
			"This media session has been revoked.", map[string]any{"action": "reauthorize"})
	case errors.Is(err, db.ErrDevicePlaybackDisabled):
		writeV1DeviceFeatureDisabled(w)
	case errors.Is(err, db.ErrDeviceUserUnavailable):
		writeV1PlaybackError(w, http.StatusForbidden, V1ErrorForbidden,
			"The account cannot use playback media sessions.", nil)
	case errors.Is(err, db.ErrPlaybackMediaSessionConflict):
		writeV1PlaybackError(w, http.StatusConflict, V1ErrorInvalidRequest,
			"The media session is busy; retry the request.",
			map[string]any{"action": "retry"})
	default:
		writeV1PlaybackError(w, http.StatusInternalServerError, V1ErrorInternal,
			"The server could not authorize the media session.", nil)
	}
}
