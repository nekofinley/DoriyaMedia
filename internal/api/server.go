package api

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"doriyamedia/internal/auth"
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

	// SubtitleAudits holds the per-library "which BDRip episodes have no
	// subtitles" audits and their last results. See subaudit.go.
	SubtitleAudits *subtitleAuditRegistry

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
	}
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
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
	if !u.CanLogin() {
		return nil
	}
	return u
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

	// static SPA
	mux.HandleFunc("/", s.serveStatic)

	return logMiddleware(mux)
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}
