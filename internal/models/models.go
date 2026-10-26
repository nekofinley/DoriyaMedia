package models

import (
	"sync"
	"time"
)

// Account roles. Only RoleAdmin reaches the settings backend; RoleUser is a
// viewer that can browse, play and keep its own playback progress.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// Account states. StatusPending is a self-registration waiting for an admin to
// approve it (only produced in the "approval" registration mode), and
// StatusDisabled blocks sign-in without deleting the account — and with it the
// playback progress attached to it.
const (
	StatusActive   = "active"
	StatusPending  = "pending"
	StatusDisabled = "disabled"
)

type User struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	PassHash string `json:"-"`
	Salt     string `json:"-"`
	Role     string `json:"role"`
	Status   string `json:"status"`
	Email    string `json:"email,omitempty"`
	// EmailVerified is false for an address that was typed but never proven.
	// An empty Email is never "unverified" — it simply was not asked for, which
	// is what keeps accounts predating this feature out of the gate.
	EmailVerified bool       `json:"email_verified"`
	CreatedAt     time.Time  `json:"created_at"`
	LastLogin     *time.Time `json:"last_login_at,omitempty"`
}

// IsAdmin reports whether the account may reach the settings backend.
func (u *User) IsAdmin() bool { return u != nil && u.Role == RoleAdmin }

// CanLogin reports whether the account is currently allowed to sign in.
func (u *User) CanLogin() bool { return u != nil && u.Status == StatusActive }

// EmailPending reports whether the account still owes proof of its address.
// Only an address that was actually collected can be pending.
func (u *User) EmailPending() bool { return u != nil && u.Email != "" && !u.EmailVerified }

type Session struct {
	Token     string    `json:"token"`
	UserID    int64     `json:"user_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Source struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"` // local | webdav | openlist
	Config    string    `json:"-"`    // raw json
	CreatedAt time.Time `json:"created_at"`
}

// SourceConfig is the union of all driver configs.
type SourceConfig struct {
	// local
	Root string `json:"root,omitempty"`
	// webdav
	WebDAVURL  string `json:"webdav_url,omitempty"`
	WebDAVUser string `json:"webdav_user,omitempty"`
	WebDAVPass string `json:"webdav_pass,omitempty"`
	// openlist / alist v3
	BaseURL string `json:"base_url,omitempty"`
	Token   string `json:"token,omitempty"`
	// bilibili (番剧 / PGC). The cookie is a full account credential — the API
	// masks it for anyone who is not an administrator.
	BiliCookie string `json:"bili_cookie,omitempty"`
	// BiliRefreshToken is what keeps a scanned login alive. A QR login hands out
	// a refresh_token beside the cookie; calling the refresh endpoint with it
	// rotates both, and a session that keeps doing that never expires. A pasted
	// browser cookie has no such token — which is exactly why it dies at the
	// browser's next rotation, however far off the expiry it carries.
	//
	// It is a credential in its own right (it can mint cookies), so it is masked
	// for anyone who is not an administrator, on the same rule as the cookie.
	BiliRefreshToken string `json:"bili_refresh_token,omitempty"`
	// There is deliberately no quality field: the picture's rung is chosen while
	// the episode plays (the player's 清晰度 menu), not fixed for a whole library.
	BiliCodec string `json:"bili_codec,omitempty"` // avc | hevc | av01 | auto
	// BiliProxy routes this source's traffic — the API calls, the CDN streams
	// and the ffmpeg that muxes them — through a proxy of its own. bilibili
	// answers -10403 (该内容在当前地区不可观看) for a request from outside the
	// licensed region, so a server abroad needs a way back in; the scraper
	// proxies are configured for TMDB / Bangumi and are often a different
	// subscription, hence a field of its own. Empty means direct.
	//
	// The same credential rule applies: a proxy string carries a password
	// (Shadowsocks' password is the key itself), so it is masked for anyone who
	// is not an administrator.
	BiliProxy string `json:"bili_proxy,omitempty"`
	// BiliPlayableOnly keeps this source's listing to what its current route
	// can actually play. bilibili licenses 番剧 per region, and the licence is
	// checked by the streaming endpoint, not by the season page: a proxy in
	// Hong Kong plays the 港澳台 titles while a mainland-only one answers
	// -10403. With this on, a season that answers -10403 is listed as empty, so
	// a media-library scan neither adds it nor keeps rows an earlier scan made
	// for it — instead of offering a card that cannot play.
	BiliPlayableOnly bool `json:"bili_playable_only,omitempty"`
}

type Library struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"` // movie | series
	SourceID  int64  `json:"source_id"`
	Path      string `json:"path"`
	Providers string `json:"providers"` // json array e.g. ["nfo","tmdb"]
	// ExcludeMode decides which exclusion list a scan uses for this library:
	// "inherit" (or empty) follows the global setting, "custom" uses Excludes —
	// including an empty Excludes, which is how a library opts out entirely.
	ExcludeMode string `json:"exclude_mode"`
	// Excludes is a json array of keywords, only read when ExcludeMode is
	// "custom". A file whose name contains one is not imported.
	Excludes  string    `json:"excludes"`
	CreatedAt time.Time `json:"created_at"`
	// Sources is every place this library scans. SourceID/Path above stay in
	// step with the first entry, so anything that only knows about one source
	// keeps working; a library with no entries at all is read as that one.
	Sources []LibrarySource `json:"sources,omitempty"`
}

// LibrarySource is one place a library scans: a storage source and the path
// inside it. A library may hold several — one show can be split across two
// drives, and a 番剧 library is often a cloud source plus a folder on the server.
type LibrarySource struct {
	LibraryID int64  `json:"library_id"`
	SourceID  int64  `json:"source_id"`
	Path      string `json:"path"`
}

type Item struct {
	ID            int64     `json:"id"`
	LibraryID     int64     `json:"library_id"`
	Type          string    `json:"type"` // movie | show | season | episode
	ParentID      *int64    `json:"parent_id,omitempty"`
	Name          string    `json:"name"`
	OriginalTitle string    `json:"original_title,omitempty"`
	Year          int       `json:"year,omitempty"`
	TMDBID        int       `json:"tmdb_id,omitempty"`
	BGMID         int       `json:"bgm_id,omitempty"` // Bangumi (bgm.tv) subject id
	Overview      string    `json:"overview,omitempty"`
	PosterPath    string    `json:"poster_path,omitempty"` // local cache file name
	BackdropPath  string    `json:"backdrop_path,omitempty"`
	StillPath     string    `json:"still_path,omitempty"`
	Rating        float64   `json:"rating,omitempty"`
	Genres        string    `json:"genres,omitempty"` // json array
	Season        int       `json:"season,omitempty"`
	Episode       int       `json:"episode,omitempty"`
	AirDate       string    `json:"air_date,omitempty"`
	AddedAt       time.Time `json:"added_at"`
	// SourceType is the kind of storage source every file of this item comes
	// from ("bilibili", "local", "webdav", …). The scan writes it, and empty
	// means either no files yet or files from more than one kind of source. The
	// UI uses it to mark bilibili content with the source's badge.
	SourceType string `json:"source_type,omitempty"`
}

type MediaFile struct {
	ID       int64     `json:"id"`
	ItemID   int64     `json:"item_id"`
	SourceID int64     `json:"source_id"`
	Path     string    `json:"path"` // driver path or absolute URL (strm)
	Size     int64     `json:"size"`
	MTime    int64     `json:"mtime"`
	Probe    string    `json:"-"` // cached probe json
	AddedAt  time.Time `json:"added_at"`
}

type Subtitle struct {
	ID          int64  `json:"id"`
	FileID      int64  `json:"file_id"`
	Path        string `json:"path"` // driver path (external) or empty
	Lang        string `json:"lang,omitempty"`
	Title       string `json:"title,omitempty"`
	Format      string `json:"format,omitempty"` // subrip|ass|webvtt|hdmv_pgs_subtitle...
	Embedded    bool   `json:"embedded"`
	StreamIdx   int    `json:"stream_idx,omitempty"`
	Convertible bool   `json:"convertible"`
}

type Progress struct {
	UserID    int64     `json:"user_id"`
	FileID    int64     `json:"file_id"`
	ItemID    int64     `json:"item_id"`
	Position  float64   `json:"position"`
	Duration  float64   `json:"duration"`
	UpdatedAt time.Time `json:"updated_at"`
}

// BangumiAccount is one viewer's own Bangumi.tv authorization. The access
// token is encrypted before it reaches the database and is never included in
// API responses.
type BangumiAccount struct {
	UserID int64 `json:"-"`
	// Generation changes whenever the viewer replaces the credential. Jobs
	// carry the generation they were queued under, so a worker that read the
	// old account cannot finish its work against a new one.
	Generation  int64      `json:"-"`
	Username    string     `json:"username"`
	Nickname    string     `json:"nickname"`
	AvatarURL   string     `json:"avatar_url,omitempty"`
	TokenEnc    string     `json:"-"`
	SyncEnabled bool       `json:"sync_enabled"`
	ConnectedAt time.Time  `json:"connected_at"`
	LastSyncAt  *time.Time `json:"last_sync_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

// BangumiSyncJob is one completed local file waiting to be marked as watched
// on Bangumi. It lives in the database so a restart or an outage can resume it.
type BangumiSyncJob struct {
	ID        int64 `json:"id"`
	UserID    int64 `json:"user_id"`
	FileID    int64 `json:"file_id"`
	SubjectID int   `json:"subject_id"`
	EpisodeID int   `json:"episode_id"`
	// EpisodeNumber is the local episode number captured at enqueue time. The
	// worker resolves the Bangumi chapter after the progress response returns.
	EpisodeNumber int       `json:"-"`
	AccountGen    int64     `json:"-"`
	Status        string    `json:"status"`
	Attempts      int       `json:"attempts"`
	LeaseToken    string    `json:"-"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	LastError     string    `json:"last_error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// BangumiSyncStatus is the account view the settings dialog consumes.
type BangumiSyncStatus struct {
	Connected   bool       `json:"connected"`
	Username    string     `json:"username,omitempty"`
	Nickname    string     `json:"nickname,omitempty"`
	AvatarURL   string     `json:"avatar_url,omitempty"`
	SyncEnabled bool       `json:"sync_enabled"`
	ConnectedAt *time.Time `json:"connected_at,omitempty"`
	LastSyncAt  *time.Time `json:"last_sync_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	Pending     int        `json:"pending"`
	Failed      int        `json:"failed"`
	Synced      int        `json:"synced"`
}

// BangumiCollectionsPage is the safe, paginated collection view used by the
// viewer's Bangumi shelf. The access token is intentionally absent: the
// account fields mirror only the public identity stored with the credential.
type BangumiCollectionsPage struct {
	Connected bool                    `json:"connected"`
	Username  string                  `json:"username"`
	Nickname  string                  `json:"nickname"`
	AvatarURL string                  `json:"avatar_url"`
	Total     int                     `json:"total"`
	Offset    int                     `json:"offset"`
	Limit     int                     `json:"limit"`
	HasMore   bool                    `json:"has_more"`
	Items     []BangumiCollectionItem `json:"items"`
}

// BangumiCollectionItem combines one Bangumi anime collection with the local
// media item that carries the same bgm_id. LocalItemID is zero when the title
// is not in the library.
type BangumiCollectionItem struct {
	SubjectID      int     `json:"subject_id"`
	Name           string  `json:"name"`
	NameCN         string  `json:"name_cn"`
	Summary        string  `json:"summary"`
	CoverURL       string  `json:"cover_url"`
	Date           string  `json:"date"`
	Eps            int     `json:"eps"`
	Score          float64 `json:"score"`
	Rank           int     `json:"rank"`
	CollectionType int     `json:"collection_type"`
	EpStatus       int     `json:"ep_status"`
	UpdatedAt      string  `json:"updated_at"`
	LocalItemID    int64   `json:"local_item_id"`
}

// ScanState is the snapshot of a scan's counters, as the settings page reads
// them. The live one is owned by ScanProgress, which copies it out under a lock.
type ScanState struct {
	LibraryID int64   `json:"library_id"`
	Running   bool    `json:"running"`
	Phase     string  `json:"phase"` // walk | organize | identify | done | error
	Total     int     `json:"total"`
	Done      int     `json:"done"`
	Current   string  `json:"current"`
	Error     string  `json:"error,omitempty"`
	StartedAt float64 `json:"started_at"`

	// walk progress
	DirsDone   int    `json:"dirs_done"`
	FilesFound int    `json:"files_found"`
	WalkErrs   int    `json:"walk_errors,omitempty"`
	FirstErr   string `json:"first_error,omitempty"`

	// Events is the live feed for the settings page, oldest first: what the scan
	// has just been doing. ScanProgress.Snapshot fills it; it is empty on a state
	// that was never attached to one.
	Events []ScanEvent `json:"events,omitempty"`

	FinishedAt float64 `json:"finished_at,omitempty"`
}

// ScanEvent is one thing a running scan just did, for the live feed the settings
// page shows. Scans are long and mostly silent, so without this the only sign of
// life is a counter; with it the log scrolls the way a build log does.
type ScanEvent struct {
	// Seq rises by one per event. The client keeps the highest it has seen and
	// appends only what is newer, so a poll that arrives twice, or a client that
	// was on another page for a while, cannot duplicate or reorder the feed.
	Seq  int64   `json:"seq"`
	At   float64 `json:"at"`
	Kind string  `json:"kind"` // dir | file | show | season | episode | sub | meta | warn
	Text string  `json:"text"`
}

// scanEventRing bounds the feed. A client polls every couple of seconds and only
// ever displays the last handful, so a hundred is generous; what matters is that
// it cannot grow without limit on a scan of a hundred thousand files.
const scanEventRing = 100

// ScanProgress is a scan's live state. Every field is written by the walking and
// organizing goroutines while the HTTP handler reads it to answer a poll, so the
// state lives behind a mutex and callers get a copy: returning the struct itself
// would hand a caller a value that keeps changing under it.
type ScanProgress struct {
	mu     sync.Mutex
	state  ScanState
	events []ScanEvent
	seq    int64
}

func NewScanProgress(libraryID int64) *ScanProgress {
	return &ScanProgress{state: ScanState{LibraryID: libraryID, Phase: "idle"}}
}

// Update mutates the state under the lock.
func (p *ScanProgress) Update(fn func(st *ScanState)) {
	p.mu.Lock()
	fn(&p.state)
	p.mu.Unlock()
}

// Set is Update for the common case of assigning whole fields.
func (p *ScanProgress) Set(st ScanState) {
	p.mu.Lock()
	p.state = st
	p.mu.Unlock()
}

// AddEvent appends one line to the live feed, dropping the oldest when full.
func (p *ScanProgress) AddEvent(kind, text string) {
	if text == "" {
		return
	}
	p.mu.Lock()
	p.seq++
	p.events = append(p.events, ScanEvent{
		Seq:  p.seq,
		At:   float64(time.Now().UnixMilli()) / 1000,
		Kind: kind,
		Text: text,
	})
	if len(p.events) > scanEventRing {
		p.events = append(p.events[:0], p.events[len(p.events)-scanEventRing:]...)
	}
	p.mu.Unlock()
}

// Snapshot is a copy of the state and the feed, safe to marshal while the scan
// keeps running. events is the last `want` lines, oldest first; want <= 0 means
// as many as the ring holds.
func (p *ScanProgress) Snapshot(want int) (ScanState, []ScanEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state
	if want <= 0 || want > len(p.events) {
		want = len(p.events)
	}
	ev := make([]ScanEvent, want)
	copy(ev, p.events[len(p.events)-want:])
	// The feed rides along with the state so one JSON object carries both: the
	// settings page polls a single endpoint for progress and the log behind it.
	st.Events = ev
	return st, ev
}
