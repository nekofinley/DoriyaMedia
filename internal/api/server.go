package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"doriyamedia/internal/auth"
	"doriyamedia/internal/bangumi"
	"doriyamedia/internal/config"
	"doriyamedia/internal/db"
	"doriyamedia/internal/media"
	"doriyamedia/internal/metadata"
	"doriyamedia/internal/models"
	"doriyamedia/internal/scan"
	"doriyamedia/internal/site"
)

type Server struct {
	Store      *db.Store
	Cfg        *config.Config
	ConfigPath string
	Scanner    *scan.Scanner
	Metadata   *metadata.Manager
	Engine     *media.Engine
	WebFS      fs.FS // embedded dist root
	DevWeb     string
	// Site holds the owner's wording / layout / footer edits. Nil disables the
	// /api/site routes (the toolbar reports the storage as unavailable).
	Site *site.Store
	// Version is the build version the linker stamped into main.version, so the
	// UI can report the binary it is actually talking to rather than a constant
	// someone has to remember to bump.
	Version string

	// loginLimiter, registerLimiter and mailLimiter bound password guessing,
	// signup floods and outbound mail. All three live in memory; see
	// attemptLimiter.
	loginLimiter    *attemptLimiter
	registerLimiter *attemptLimiter
	mailLimiter     *attemptLimiter
	// Device-auth limits are separate so a polling client cannot exhaust the
	// login or mail budgets. Fresh instances use New; nil remains permissive for
	// small zero-value Server tests.
	deviceAuthStartLimiter    *attemptLimiter
	deviceAuthTokenLimiter    *attemptLimiter
	deviceAuthRefreshLimiter  *attemptLimiter
	deviceAuthLookupLimiter   *attemptLimiter
	deviceAuthDecisionLimiter *attemptLimiter

	// SubtitleAudits holds the per-library "which BDRip episodes have no
	// subtitles" audits and their last results. See subaudit.go.
	SubtitleAudits *subtitleAuditRegistry
	// BangumiSync holds each viewer's own Bangumi credential and the durable
	// watch-progress outbox. See bangumi_handlers.go.
	BangumiSync *bangumi.Service

	// imports holds "search a bilibili title, click it, import it" jobs. They are
	// short-lived and in memory: a job is only useful while its loading page is
	// polling. See catalog_handlers.go.
	importsMu sync.Mutex
	imports   map[string]*importJob
}

func New(store *db.Store, cfg *config.Config, scanner *scan.Scanner, meta *metadata.Manager, engine *media.Engine, webFS fs.FS, version string) *Server {
	return &Server{
		Store: store, Cfg: cfg, Scanner: scanner, Metadata: meta, Engine: engine,
		WebFS: webFS, DevWeb: cfg.DevWebDir, Version: version,
		SubtitleAudits: newSubtitleAuditRegistry(),
		BangumiSync:    bangumi.NewService(store, meta, cfg),
		imports:        map[string]*importJob{},
		// Ten wrong passwords per address per five minutes is far more than a
		// person needs and far too slow to guess with; five signups per hour
		// keeps a shared address (a family, a dorm) working while stopping a
		// script from filling the user table. Five messages per hour applies
		// the same reasoning to the mail server: enough to get yourself back
		// in, not enough to be worth abusing.
		loginLimiter:    newAttemptLimiter(10, 5*time.Minute),
		registerLimiter: newAttemptLimiter(5, time.Hour),
		mailLimiter:     newAttemptLimiter(5, time.Hour),
		// A native client polls every five seconds. Token/refresh allowances
		// leave room for retries while lookup and decision operations are much
		// stricter because they require an interactive Web session.
		deviceAuthStartLimiter:    newAttemptLimiter(20, 10*time.Minute),
		deviceAuthTokenLimiter:    newAttemptLimiter(60, time.Minute),
		deviceAuthRefreshLimiter:  newAttemptLimiter(20, time.Minute),
		deviceAuthLookupLimiter:   newAttemptLimiter(30, time.Minute),
		deviceAuthDecisionLimiter: newAttemptLimiter(10, time.Minute),
	}
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	_ = writeJSONResponse(w, status, v)
}

func writeJSONResponse(w http.ResponseWriter, status int, v any) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(v); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	payload := body.Bytes()
	n, err := w.Write(payload)
	if err != nil {
		return err
	}
	if n != len(payload) {
		return io.ErrShortWrite
	}
	return nil
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readBody(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// ---------- auth ----------

const cookieName = "doriyamedia_token"

// sessionToken reads the caller's session token from the cookie, or from a
// bearer header for clients that cannot hold cookies.
func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		return c.Value
	}
	if ah := r.Header.Get("Authorization"); strings.HasPrefix(ah, "Bearer ") {
		return ah[len("Bearer "):]
	}
	return ""
}

func (s *Server) currentUser(r *http.Request) *models.User {
	token := sessionToken(r)
	if token == "" {
		return nil
	}
	sess, err := s.Store.SessionByToken(token)
	if err != nil || time.Now().After(sess.ExpiresAt) {
		return nil
	}
	u, err := s.Store.UserByID(sess.UserID)
	if err != nil {
		return nil
	}
	// A session that outlived a "禁用" would keep working, so the account state
	// is re-checked on every request rather than only at sign-in.
	if !s.userAllowedForNativeClient(u) {
		return nil
	}
	return u
}

// userAllowedForNativeClient is the shared live-account gate for both Web
// sessions and device access. An unverified address only blocks while mail
// verification is actually required; legacy accounts without an address keep
// their previous behavior.
func (s *Server) userAllowedForNativeClient(u *models.User) bool {
	if u == nil || !u.CanLogin() {
		return false
	}
	return !(s.Cfg != nil && s.Cfg.EmailVerificationRequired() && u.EmailPending())
}

type authedHandler func(w http.ResponseWriter, r *http.Request, user *models.User)

func (s *Server) requireAuth(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.currentUser(r)
		if u == nil {
			fail(w, 401, "未登录")
			return
		}
		h(w, r, u)
	}
}

func (s *Server) requireAdmin(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.currentUser(r)
		if u == nil {
			fail(w, 401, "未登录")
			return
		}
		if u.Role != "admin" {
			fail(w, 403, "需要管理员权限")
			return
		}
		h(w, r, u)
	}
}

// ---------- signed URLs (for players that cannot send headers) ----------

func signPayload(fileID int64, exp int64) string {
	return fmt.Sprintf("%d|%d", fileID, exp)
}

func signedQuery(secret string, fileID int64, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	sig := auth.Sign(secret, signPayload(fileID, exp))
	return fmt.Sprintf("?exp=%d&sig=%s", exp, sig)
}

// sigOK validates a signed URL for fileID.
func (s *Server) sigOK(r *http.Request, fileID int64) bool {
	q := r.URL.Query()
	sig := q.Get("sig")
	exp := q.Get("exp")
	if sig == "" || exp == "" {
		return false
	}
	var expN int64
	if _, err := fmt.Sscanf(exp, "%d", &expN); err != nil {
		return false
	}
	if time.Now().Unix() > expN {
		return false
	}
	return auth.VerifySig(s.Cfg.Secret, signPayload(fileID, expN), sig)
}

// allowPlayerAuth: session OR valid signature (players cannot send cookies/headers on 302 targets).
func (s *Server) allowPlayerAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.currentUser(r) != nil {
			h(w, r)
			return
		}
		// extract file id from path segment
		id, ok := pathID(r)
		if ok && s.sigOK(r, id) {
			h(w, r)
			return
		}
		fail(w, 401, "签名无效或已过期")
	}
}

func pathID(r *http.Request) (int64, bool) {
	v := r.PathValue("id")
	if v == "" {
		return 0, false
	}
	var id int64
	if _, err := fmt.Sscanf(v, "%d", &id); err != nil {
		return 0, false
	}
	return id, true
}

// ---------- router ----------

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// API v1 discovery stays public even while every feature flag is disabled.
	mux.HandleFunc("GET /api/v1/meta", s.handleV1Meta)

	// Device authentication. Each handler checks both client_v1_enabled and
	// device_auth_enabled before touching credentials or storage.
	mux.HandleFunc("POST /api/v1/auth/device/authorizations", s.handleV1DeviceAuthorizationStart)
	mux.HandleFunc("GET /api/v1/auth/device/authorizations/lookup", s.handleV1DeviceAuthorizationLookup)
	mux.HandleFunc("GET /api/v1/auth/device/authorizations/{id}", s.handleV1DeviceAuthorizationStatus)
	mux.HandleFunc("POST /api/v1/auth/device/authorizations/{id}/decision", s.handleV1DeviceAuthorizationDecision)
	mux.HandleFunc("POST /api/v1/auth/device/token", s.handleV1DeviceToken)
	mux.HandleFunc("POST /api/v1/auth/refresh", s.handleV1DeviceRefresh)
	mux.HandleFunc("POST /api/v1/auth/logout", s.requireV1DeviceAccess(s.handleV1DeviceLogout))
	mux.HandleFunc("GET /api/v1/devices", s.requireV1DeviceAccess(s.handleV1DeviceList))
	mux.HandleFunc("DELETE /api/v1/devices/{id}", s.requireV1DeviceAccess(s.handleV1DeviceDelete))

	// Playback tickets and capability-addressed media. The media-session routes
	// intentionally do not accept a Bearer token: the high-entropy path
	// capability is the media credential and is redacted by the log middleware.
	mux.HandleFunc("POST /api/v1/playback/tickets", s.requireV1DeviceAccess(s.handleV1PlaybackTicketCreate))
	mux.HandleFunc("GET /api/v1/playback/tickets/{id}", s.requireV1DeviceAccess(s.handleV1PlaybackTicketStatus))
	mux.HandleFunc("POST /api/v1/playback/tickets/{id}/refresh", s.requireV1DeviceAccess(s.handleV1PlaybackTicketRefresh))
	mux.HandleFunc("GET /api/v1/playback/media-sessions/{id}/stream", s.handleV1PlaybackMediaStream)
	mux.HandleFunc("HEAD /api/v1/playback/media-sessions/{id}/stream", s.handleV1PlaybackMediaStream)
	mux.HandleFunc("GET /api/v1/playback/media-sessions/{id}/index.m3u8", s.handleV1ServerHLSPlaylist)
	mux.HandleFunc("GET /api/v1/playback/media-sessions/{media}/subtitle-sessions/{sub}", s.handleV1PlaybackHLSSubtitle)
	mux.HandleFunc("HEAD /api/v1/playback/media-sessions/{media}/subtitle-sessions/{sub}", s.handleV1PlaybackHLSSubtitle)
	mux.HandleFunc("GET /api/v1/playback/media-sessions/{id}/{segment}", s.handleV1ServerHLSSegment)
	mux.HandleFunc("GET /api/v1/playback/subtitle-sessions/{id}/subtitle.vtt", s.handleV1PlaybackSubtitle)
	mux.HandleFunc("HEAD /api/v1/playback/subtitle-sessions/{id}/subtitle.vtt", s.handleV1PlaybackSubtitle)

	// auth / setup
	mux.HandleFunc("POST /api/setup", s.handleSetup)
	mux.HandleFunc("POST /api/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.requireAuth(s.handleLogout))
	mux.HandleFunc("GET /api/auth/me", s.requireAuth(s.handleMe))
	mux.HandleFunc("PUT /api/auth/profile", s.requireAuth(s.handleUpdateProfile))
	mux.HandleFunc("POST /api/auth/password", s.requireAuth(s.handleChangePassword))
	mux.HandleFunc("GET /api/bootstrap", s.handleBootstrap)

	// email verification and password recovery (public: the links arrive in a
	// mailbox, so there is no session to present yet)
	mux.HandleFunc("POST /api/auth/verify-email", s.handleVerifyEmail)
	mux.HandleFunc("POST /api/auth/resend-verification", s.handleResendVerification)
	mux.HandleFunc("POST /api/auth/forgot-password", s.handleForgotPassword)
	mux.HandleFunc("POST /api/auth/reset-password", s.handleResetPassword)
	mux.HandleFunc("PUT /api/auth/email", s.requireAuth(s.handleSetOwnEmail))
	mux.HandleFunc("POST /api/auth/email/resend", s.requireAuth(s.handleResendOwnVerification))

	// user management (admin only)
	mux.HandleFunc("GET /api/admin/users", s.requireAdmin(s.handleListUsers))
	mux.HandleFunc("POST /api/admin/users", s.requireAdmin(s.handleCreateUser))
	mux.HandleFunc("PUT /api/admin/users/{id}", s.requireAdmin(s.handleUpdateUser))
	mux.HandleFunc("DELETE /api/admin/users/{id}", s.requireAdmin(s.handleDeleteUser))
	mux.HandleFunc("POST /api/admin/users/{id}/password", s.requireAdmin(s.handleSetUserPassword))
	mux.HandleFunc("POST /api/admin/users/{id}/revoke", s.requireAdmin(s.handleRevokeUserSessions))
	mux.HandleFunc("POST /api/admin/mail/test", s.requireAdmin(s.handleMailTest))
	mux.HandleFunc("GET /api/admin/client-devices", s.requireAdmin(s.handleAdminListClientDevices))
	mux.HandleFunc("PUT /api/admin/client-devices/{id}/features", s.requireAdmin(s.handleAdminUpdateClientDeviceFeatures))

	// system
	mux.HandleFunc("GET /api/system/info", s.requireAuth(s.handleSystemInfo))
	mux.HandleFunc("GET /api/config", s.requireAdmin(s.handleGetConfig))
	mux.HandleFunc("PUT /api/config", s.requireAdmin(s.handlePutConfig))
	mux.HandleFunc("POST /api/metadata/test", s.requireAdmin(s.handleMetadataTest))

	// sources
	mux.HandleFunc("GET /api/sources", s.requireAuth(s.handleListSources))
	mux.HandleFunc("POST /api/sources", s.requireAdmin(s.handleCreateSource))
	mux.HandleFunc("PUT /api/sources/{id}", s.requireAdmin(s.handleUpdateSource))
	mux.HandleFunc("DELETE /api/sources/{id}", s.requireAdmin(s.handleDeleteSource))
	mux.HandleFunc("POST /api/sources/test", s.requireAdmin(s.handleTestSource))
	mux.HandleFunc("POST /api/sources/test-proxy", s.requireAdmin(s.handleTestProxy))

	// bilibili account: who the cookie belongs to, and how to get one without a
	// browser. A pasted cookie dies when the browser rotates it, and the failure
	// is silent (480P, "needs a membership") — these routes make it visible and
	// fixable. See bili_account_handlers.go.
	mux.HandleFunc("GET /api/sources/{id}/bili-account", s.requireAdmin(s.handleBiliAccount))
	mux.HandleFunc("POST /api/sources/{id}/bili-qr", s.requireAdmin(s.handleBiliQR))
	mux.HandleFunc("POST /api/sources/{id}/bili-qr/poll", s.requireAdmin(s.handleBiliQRPoll))
	mux.HandleFunc("POST /api/sources/{id}/bili-refresh", s.requireAdmin(s.handleBiliRefresh))

	// libraries
	mux.HandleFunc("GET /api/libraries", s.requireAuth(s.handleListLibraries))
	mux.HandleFunc("POST /api/libraries", s.requireAdmin(s.handleCreateLibrary))
	mux.HandleFunc("PUT /api/libraries/{id}", s.requireAdmin(s.handleUpdateLibrary))
	mux.HandleFunc("DELETE /api/libraries/{id}", s.requireAdmin(s.handleDeleteLibrary))
	mux.HandleFunc("POST /api/libraries/{id}/scan", s.requireAdmin(s.handleScanLibrary))
	mux.HandleFunc("POST /api/libraries/{id}/scan/stop", s.requireAdmin(s.handleStopScan))
	mux.HandleFunc("GET /api/scan/status", s.requireAuth(s.handleScanStatus))

	// subtitle audit (which BDRip episodes have no subtitles at all)
	mux.HandleFunc("POST /api/libraries/{id}/subtitle-audit", s.requireAdmin(s.handleSubtitleAuditStart))
	mux.HandleFunc("POST /api/libraries/{id}/subtitle-audit/stop", s.requireAdmin(s.handleSubtitleAuditStop))
	mux.HandleFunc("GET /api/subtitle-audit", s.requireAuth(s.handleSubtitleAuditStatus))

	// items
	mux.HandleFunc("GET /api/home", s.requireAuth(s.handleHome))
	mux.HandleFunc("GET /api/items", s.requireAuth(s.handleListItems))
	mux.HandleFunc("GET /api/items/{id}", s.requireAuth(s.handleItemDetail))
	mux.HandleFunc("POST /api/items/{id}/refresh", s.requireAuth(s.handleItemRefresh))
	mux.HandleFunc("GET /api/img/{id}/{kind}", s.handleImage)

	// catalogue search → import (a bilibili title the library has not scanned)
	mux.HandleFunc("GET /api/catalog/search", s.requireAuth(s.handleCatalogSearch))
	mux.HandleFunc("POST /api/catalog/import", s.requireAuth(s.handleCatalogImport))
	mux.HandleFunc("GET /api/import/{id}", s.requireAuth(s.handleImportStatus))

	// viewer wishlist: search a title, submit it, and let an administrator work
	// through the pending poster wall. The two listing routes deliberately have
	// different scopes: a viewer sees only their own requests, an admin sees all.
	mux.HandleFunc("GET /api/requests/search", s.requireAuth(s.handleRequestSearch))
	mux.HandleFunc("GET /api/requests", s.requireAuth(s.handleListMediaRequests))
	mux.HandleFunc("POST /api/requests", s.requireAuth(s.handleCreateMediaRequest))
	mux.HandleFunc("GET /api/admin/requests", s.requireAdmin(s.handleAdminListMediaRequests))
	mux.HandleFunc("PUT /api/admin/requests/{id}", s.requireAdmin(s.handleUpdateMediaRequest))
	mux.HandleFunc("DELETE /api/admin/requests/{id}", s.requireAdmin(s.handleDeleteMediaRequest))
	mux.HandleFunc("POST /api/admin/requests/{id}/import", s.requireAdmin(s.handleRequestImport))

	// playback
	mux.HandleFunc("GET /api/play/{id}", s.requireAuth(s.handlePlay))
	mux.HandleFunc("GET /api/play/{id}/status", s.requireAuth(s.handlePlayStatus))
	mux.HandleFunc("GET /api/browse/{id}", s.requireAuth(s.handleBrowse))
	mux.HandleFunc("GET /api/browseplay/{id}", s.requireAuth(s.handleBrowsePlay))
	mux.HandleFunc("GET /api/stream/{id}", s.allowPlayerAuth(s.handleStream))
	mux.HandleFunc("HEAD /api/stream/{id}", s.allowPlayerAuth(s.handleStream))
	mux.HandleFunc("GET /api/hls/{id}/{rest...}", s.allowPlayerAuth(s.handleHLS))
	mux.HandleFunc("GET /api/subs/{id}/{sub}", s.allowPlayerAuth(s.handleSubtitle))
	mux.HandleFunc("GET /api/danmaku/{id}/comments.json", s.allowPlayerAuth(s.handleDanmaku))

	// site content (login page wording/layout, footer) — read by every visitor,
	// written only by the admin from the editing toolbar
	mux.HandleFunc("GET /api/site", s.handleGetSite)
	mux.HandleFunc("PUT /api/site", s.requireAdmin(s.handlePutSite))
	mux.HandleFunc("DELETE /api/site", s.requireAdmin(s.handleResetSite))

	// progress
	mux.HandleFunc("POST /api/progress", s.requireAuth(s.handleSaveProgress))
	mux.HandleFunc("GET /api/progress", s.requireAuth(s.handleListProgress))
	mux.HandleFunc("GET /api/bangumi/account", s.requireAuth(s.handleBangumiAccount))
	mux.HandleFunc("PUT /api/bangumi/account", s.requireAuth(s.handleBangumiConnect))
	mux.HandleFunc("DELETE /api/bangumi/account", s.requireAuth(s.handleBangumiDisconnect))
	mux.HandleFunc("PUT /api/bangumi/account/settings", s.requireAuth(s.handleBangumiSettings))
	mux.HandleFunc("POST /api/bangumi/account/sync", s.requireAuth(s.handleBangumiBackfill))
	mux.HandleFunc("GET /api/bangumi/collections", s.requireAuth(s.handleBangumiCollections))
	mux.HandleFunc("GET /api/bangumi/cover", s.requireAuth(s.handleBangumiCover))

	// static SPA
	mux.HandleFunc("/", s.serveStatic)

	return logMiddleware(mux)
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s %s", r.Method, redactAPILogPath(r.URL.Path),
				time.Since(start).Round(time.Millisecond))
		}
	})
}

func redactAPILogPath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		switch {
		case strings.HasPrefix(part, "ms_"):
			parts[i] = "ms_redacted"
		case strings.HasPrefix(part, "ss_"):
			parts[i] = "ss_redacted"
		case strings.HasPrefix(part, "pt_"):
			parts[i] = "pt_redacted"
		}
	}
	return strings.Join(parts, "/")
}
