package models

import (
	"encoding/json"
	"time"
)

// Playback ticket states. A ticket starts in probing and only advances through
// state-changing store methods guarded by a version check.
const (
	PlaybackTicketProbing     = "probing"
	PlaybackTicketReady       = "ready"
	PlaybackTicketConsuming   = "consuming"
	PlaybackTicketUnsupported = "unsupported"
	PlaybackTicketFailed      = "failed"
	PlaybackTicketExpired     = "expired"
	PlaybackTicketRevoked     = "revoked"
	PlaybackTicketExhausted   = "exhausted"
)

const (
	PlaybackPurposePlayback = "playback"

	PlaybackTransportDirectHTTP  = "direct-http"
	PlaybackTransportServerProxy = "server-proxy"
	PlaybackTransportServerHLS   = "server-hls"

	PlaybackMediaSessionActive  = "active"
	PlaybackMediaSessionExpired = "expired"
	PlaybackMediaSessionRevoked = "revoked"

	PlaybackSubtitleSessionActive  = "active"
	PlaybackSubtitleSessionExpired = "expired"
	PlaybackSubtitleSessionRevoked = "revoked"
)

const (
	PlaybackTicketTTL                  = 5 * time.Minute
	PlaybackTransportClaimTTL          = 30 * time.Second
	PlaybackMediaSessionIdleTTL        = 15 * time.Minute
	PlaybackMediaSessionAbsoluteTTL    = 6 * time.Hour
	PlaybackSubtitleSessionIdleTTL     = 15 * time.Minute
	PlaybackSubtitleSessionAbsoluteTTL = 6 * time.Hour
	PlaybackMaxSubtitleGrants          = 32
	// PlaybackVideoIndexUnknown is the migration-safe sentinel for playback
	// sessions created before the selected video stream was frozen. It must
	// never be used to select a stream for new playback.
	PlaybackVideoIndexUnknown = -1
)

// PlaybackTicketBinding is required on every ticket lookup and mutation. Public
// ticket ids are identifiers, not bearer credentials.
type PlaybackTicketBinding struct {
	UserID   int64
	DeviceID int64
	FileID   int64
	TicketID string
}

// PlaybackTicketRequest starts a short-lived playback decision. The store
// generates the public ticket id and the five-minute absolute expiry.
type PlaybackTicketRequest struct {
	UserID              int64
	DeviceID            int64
	FileID              int64
	Purpose             string
	CandidateTransports []string
	MediaSummary        json.RawMessage
	// ClientCapabilities is immutable playback input used when a later GET or
	// refresh advances a probing ticket. It is never exposed by the status API.
	ClientCapabilities json.RawMessage
}

// PlaybackTicketProbeResult is the terminal outcome of ticket probing. It may
// only move a probing ticket to ready, unsupported, or failed.
type PlaybackTicketProbeResult struct {
	State               string
	CandidateTransports []string
	MediaSummary        json.RawMessage
	SubtitleIDs         []int64
	FailureCode         string
	// VideoIndex is the exact ffprobe stream index selected when the ticket
	// becomes ready. It is internal state and is never exposed to clients.
	VideoIndex *int
}

// PlaybackTicket contains no source URL, source headers, or capabilities.
type PlaybackTicket struct {
	ID                  int64           `json:"-"`
	TicketID            string          `json:"ticket_id"`
	UserID              int64           `json:"user_id"`
	DeviceID            int64           `json:"device_id"`
	FileID              int64           `json:"file_id"`
	Purpose             string          `json:"purpose"`
	State               string          `json:"state"`
	CandidateTransports []string        `json:"candidate_transports"`
	AttemptedTransports []string        `json:"attempted_transports"`
	SelectedTransport   string          `json:"selected_transport,omitempty"`
	MediaSummary        json.RawMessage `json:"media_summary"`
	ClientCapabilities  json.RawMessage `json:"-"`
	SubtitleIDs         []int64         `json:"-"`
	VideoIndex          int             `json:"-"`
	// ClaimToken and ClaimExpiresAt form a short internal lease while a
	// transport is being issued. They are never exposed to clients.
	ClaimToken     string     `json:"-"`
	ClaimExpiresAt *time.Time `json:"-"`
	FailureCode    string     `json:"failure_code,omitempty"`
	Version        int64      `json:"version"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ConsumedAt     *time.Time `json:"consumed_at,omitempty"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
}

// PlaybackMediaSessionBinding is required for session lookup, touch, and
// revocation. All fields must match the stored row.
type PlaybackMediaSessionBinding struct {
	Ticket    PlaybackTicketBinding
	Transport string
}

// PlaybackMediaSessionRequest creates a server-side media authorization. The
// playback parameters are fixed here and never read from request queries.
type PlaybackMediaSessionRequest struct {
	Ticket          PlaybackTicketBinding
	Transport       string
	Mode            string
	AudioOrdinal    int
	StartSeconds    float64
	Quality         *string
	TranscodeHeight *int
}

// PlaybackMediaSession is persisted without the plaintext capability.
type PlaybackMediaSession struct {
	ID                int64      `json:"-"`
	TicketID          string     `json:"ticket_id"`
	UserID            int64      `json:"user_id"`
	DeviceID          int64      `json:"device_id"`
	FileID            int64      `json:"file_id"`
	Purpose           string     `json:"purpose"`
	Transport         string     `json:"transport"`
	State             string     `json:"state"`
	Mode              string     `json:"mode"`
	AudioOrdinal      int        `json:"audio_ordinal"`
	VideoIndex        int        `json:"-"`
	StartSeconds      float64    `json:"start_seconds"`
	Quality           *string    `json:"quality,omitempty"`
	TranscodeHeight   *int       `json:"transcode_height,omitempty"`
	Version           int64      `json:"version"`
	CreatedAt         time.Time  `json:"created_at"`
	LastUsedAt        *time.Time `json:"last_used_at,omitempty"`
	IdleExpiresAt     time.Time  `json:"idle_expires_at"`
	AbsoluteExpiresAt time.Time  `json:"absolute_expires_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
}

// PlaybackMediaSessionGrant returns the plaintext capability exactly once. The
// database stores only its SHA-256 hash.
type PlaybackMediaSessionGrant struct {
	Session    *PlaybackMediaSession `json:"session"`
	Capability string                `json:"capability"`
}

// PlaybackSubtitleSessionRequest creates a short-lived capability for one
// subtitle grant on one already-issued playback transport.
type PlaybackSubtitleSessionRequest struct {
	Ticket       PlaybackTicketBinding
	Transport    string
	SubtitleID   int64
	StartSeconds float64
}

// PlaybackSubtitleSession is persisted without the plaintext capability. The
// subtitle id and timeline origin are immutable after creation.
type PlaybackSubtitleSession struct {
	ID                int64      `json:"-"`
	TicketID          string     `json:"ticket_id"`
	UserID            int64      `json:"user_id"`
	DeviceID          int64      `json:"device_id"`
	FileID            int64      `json:"file_id"`
	Purpose           string     `json:"purpose"`
	Transport         string     `json:"transport"`
	SubtitleID        int64      `json:"subtitle_id"`
	State             string     `json:"state"`
	StartSeconds      float64    `json:"start_seconds"`
	Version           int64      `json:"version"`
	CreatedAt         time.Time  `json:"created_at"`
	LastUsedAt        *time.Time `json:"last_used_at,omitempty"`
	IdleExpiresAt     time.Time  `json:"idle_expires_at"`
	AbsoluteExpiresAt time.Time  `json:"absolute_expires_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
}

// PlaybackSubtitleSessionGrant returns the plaintext subtitle capability once.
// The database stores only its SHA-256 hash.
type PlaybackSubtitleSessionGrant struct {
	Session    *PlaybackSubtitleSession `json:"session"`
	Capability string                   `json:"capability"`
}
