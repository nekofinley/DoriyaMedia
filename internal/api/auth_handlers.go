package api

import (
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"doriyamedia/internal/auth"
	"doriyamedia/internal/config"
	"doriyamedia/internal/models"
)

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.UserCount()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"setup_needed": n == 0,
		// The sign-in page renders its tabs and its form from this: setup,
		// sign-in, signup, whether an address is required, and whether a
		// CAPTCHA has to be solved first.
		"registration":  s.Cfg.Users.Registration,
		"can_register":  n > 0 && s.Cfg.RegistrationAllowed() && !(s.Cfg.Users.RequireEmail && !s.Cfg.MailEnabled()),
		"min_password":  s.passwordMin(),
		"require_email": s.Cfg.EmailVerificationRequired(),
		"mail_enabled":  s.Cfg.MailEnabled(),
		"turnstile": map[string]any{
			"enabled":  s.Cfg.TurnstileEnabled(),
			"site_key": s.Cfg.Turnstile.SiteKey,
		},
	})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.UserCount()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if n > 0 {
		fail(w, 400, "已初始化,请直接登录")
		return
	}
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	name, err := validateName(req.Name)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if err := s.validatePassword(req.Password); err != nil {
		fail(w, 400, err.Error())
		return
	}
	hash, salt := auth.HashPassword(req.Password)
	u := &models.User{
		Name: name, PassHash: hash, Salt: salt,
		Role: models.RoleAdmin, Status: models.StatusActive,
	}
	if err := s.Store.CreateUser(u); err != nil {
		fail(w, 500, err.Error())
		return
	}
	s.startSession(w, u)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name           string `json:"name"`
		Password       string `json:"password"`
		TurnstileToken string `json:"turnstile_token"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	key := clientIP(r)
	if !s.loginLimiter.Allow(key) {
		fail(w, 429, "尝试过于频繁,请稍后再试")
		return
	}
	// The challenge is checked before the password is even looked at: that is
	// what makes each guess cost a solved challenge rather than a hash.
	if err := s.verifyTurnstile(r, req.TurnstileToken); err != nil {
		if wait := s.loginLimiter.Hit(key); wait > 0 {
			fail(w, 429, fmt.Sprintf("尝试过于频繁,请 %d 秒后再试", wait))
			return
		}
		fail(w, 400, err.Error())
		return
	}
	// An unknown name and a wrong password are answered identically, and both
	// count against the limiter, so the form cannot be used to discover which
	// accounts exist.
	u, err := s.Store.UserByName(strings.TrimSpace(req.Name))
	if err != nil || !auth.Verify(req.Password, u.PassHash, u.Salt) {
		if wait := s.loginLimiter.Hit(key); wait > 0 {
			fail(w, 429, fmt.Sprintf("密码错误次数过多,请 %d 秒后再试", wait))
			return
		}
		fail(w, 401, "用户名或密码错误")
		return
	}
	// The password is right, so telling the owner why the account is blocked
	// reveals nothing to anyone else.
	switch u.Status {
	case models.StatusPending:
		fail(w, 403, "账号正在等待管理员审核通过")
		return
	case models.StatusDisabled:
		fail(w, 403, "账号已被管理员禁用")
		return
	}
	// Only an address that was actually collected can hold an account back, so
	// accounts predating this feature are unaffected. Turning the setting off
	// again lets everyone through, which is the way out if the mail server dies.
	if s.Cfg.EmailVerificationRequired() && u.EmailPending() {
		fail(w, 403, "请先完成邮箱验证:在登录页点「重新发送验证邮件」")
		return
	}
	s.loginLimiter.Reset(key)
	s.startSession(w, u)
}

func (s *Server) startSession(w http.ResponseWriter, u *models.User) {
	token := auth.NewToken()
	exp := time.Now().Add(sessionTTL)
	if err := s.Store.CreateSession(token, u.ID, exp); err != nil {
		fail(w, 500, err.Error())
		return
	}
	// Every session starts here — setup, signup and sign-in alike — so this is
	// the one place that has to record the visit. Registering and being signed
	// straight in is a login too, and the user table should not call it "never".
	now := time.Now()
	_ = s.Store.TouchLastLogin(u.ID, now)
	u.LastLogin = &now
	// Sessions the server never used again would otherwise accumulate for the
	// lifetime of the database; signing in is a cheap place to sweep them.
	_ = s.Store.PruneSessions()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Expires: exp,
	})
	writeJSON(w, 200, map[string]any{"token": token, "user": u})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, user *models.User) {
	_ = s.Store.DeleteSession(sessionToken(r))
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleMe tells the UI who it is talking to. The role decides whether the
// settings backend is reachable at all, so the client can route on it.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, user *models.User) {
	writeJSON(w, 200, map[string]any{
		"user":         user,
		"is_admin":     user.IsAdmin(),
		"setup_needed": false,
	})
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request, user *models.User) {
	ffmpegOK := exec.Command(s.Cfg.FFmpeg, "-version").Run() == nil
	ffprobeOK := exec.Command(s.Cfg.FFProbe, "-version").Run() == nil
	// An unstamped build says "dev", the same word `-version` prints, rather than
	// a release number it cannot know.
	version := s.Version
	if version == "" {
		version = "dev"
	}
	writeJSON(w, 200, map[string]any{
		"version":    version,
		"os":         runtime.GOOS,
		"ffmpeg_ok":  ffmpegOK,
		"ffprobe_ok": ffprobeOK,
		"tmdb_ok":    s.Cfg.TMDB.APIKey != "",
		"data_dir":   s.Cfg.Abs(),
		"force_mode": s.Cfg.Playback.ForceMode,
	})
}

func (s *Server) configFile() string {
	if s.ConfigPath != "" {
		return s.ConfigPath
	}
	return "config.json"
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request, user *models.User) {
	cfg := map[string]any{
		"port":             s.Cfg.Port,
		"ffmpeg":           s.Cfg.FFmpeg,
		"ffprobe":          s.Cfg.FFProbe,
		"scan_concurrency": s.Cfg.ScanConcurrency,
		"exclude_keywords": s.Cfg.ExcludeKeywords,
		"dev_web_dir":      s.Cfg.DevWebDir,
		"tmdb":             s.Cfg.TMDB,
		"bgm":              s.Cfg.BGM,
		"playback":         s.Cfg.Playback,
		"users":            s.Cfg.Users,
		"smtp":             s.Cfg.SMTP,
		"turnstile":        s.Cfg.Turnstile,
	}
	writeJSON(w, 200, cfg)
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request, user *models.User) {
	var req struct {
		Port            *int             `json:"port"`
		FFmpeg          *string          `json:"ffmpeg"`
		FFProbe         *string          `json:"ffprobe"`
		ScanConcurrency *int             `json:"scan_concurrency"`
		ExcludeKeywords *[]string        `json:"exclude_keywords"`
		DevWebDir       *string          `json:"dev_web_dir"`
		TMDB            *configTMDB      `json:"tmdb"`
		BGM             *configBGM       `json:"bgm"`
		Playback        *configPlayback  `json:"playback"`
		Users           *configUsers     `json:"users"`
		SMTP            *configSMTP      `json:"smtp"`
		Turnstile       *configTurnstile `json:"turnstile"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, err.Error())
		return
	}
	if req.Port != nil && *req.Port > 0 {
		s.Cfg.Port = *req.Port
	}
	if req.FFmpeg != nil && *req.FFmpeg != "" {
		s.Cfg.FFmpeg = *req.FFmpeg
	}
	if req.FFProbe != nil && *req.FFProbe != "" {
		s.Cfg.FFProbe = *req.FFProbe
	}
	if req.ScanConcurrency != nil && *req.ScanConcurrency > 0 {
		s.Cfg.ScanConcurrency = *req.ScanConcurrency
	}
	// A list is saved as sent, including an empty one: the defaults are written into
	// the config file so that deleting an entry is how a user turns it off.
	if req.ExcludeKeywords != nil {
		s.Cfg.ExcludeKeywords = cleanKeywords(*req.ExcludeKeywords)
	}
	if req.DevWebDir != nil {
		s.Cfg.DevWebDir = *req.DevWebDir
	}
	if req.TMDB != nil {
		s.Cfg.TMDB.APIKey = req.TMDB.APIKey
		if req.TMDB.Language != "" {
			s.Cfg.TMDB.Language = req.TMDB.Language
		}
		if req.TMDB.APIBase != "" {
			s.Cfg.TMDB.APIBase = req.TMDB.APIBase
		}
		if req.TMDB.ImgBase != "" {
			s.Cfg.TMDB.ImgBase = req.TMDB.ImgBase
		}
		if req.TMDB.UseProxy != nil {
			s.Cfg.TMDB.UseProxy = *req.TMDB.UseProxy
		}
		if req.TMDB.Proxy != nil && strings.TrimSpace(*req.TMDB.Proxy) != "" {
			s.Cfg.TMDB.Proxy = strings.TrimSpace(*req.TMDB.Proxy)
		}
	}
	if req.BGM != nil {
		s.Cfg.BGM.Token = req.BGM.Token
		if req.BGM.APIBase != "" {
			s.Cfg.BGM.APIBase = strings.TrimSpace(req.BGM.APIBase)
		}
		if req.BGM.UseProxy != nil {
			s.Cfg.BGM.UseProxy = *req.BGM.UseProxy
		}
		if req.BGM.Proxy != nil && strings.TrimSpace(*req.BGM.Proxy) != "" {
			s.Cfg.BGM.Proxy = strings.TrimSpace(*req.BGM.Proxy)
		}
	}
	if req.Playback != nil {
		if req.Playback.ForceMode != "" {
			s.Cfg.Playback.ForceMode = req.Playback.ForceMode
		}
		if req.Playback.SegmentSeconds > 0 {
			s.Cfg.Playback.SegmentSeconds = req.Playback.SegmentSeconds
		}
		if req.Playback.TranscodeHeight > 0 {
			s.Cfg.Playback.TranscodeHeight = req.Playback.TranscodeHeight
		}
		if req.Playback.ProbeSizeMB != nil && *req.Playback.ProbeSizeMB > 0 {
			s.Cfg.Playback.ProbeSizeMB = *req.Playback.ProbeSizeMB
		}
		if req.Playback.ProbeAnalyzeSec != nil && *req.Playback.ProbeAnalyzeSec > 0 {
			s.Cfg.Playback.ProbeAnalyzeSec = *req.Playback.ProbeAnalyzeSec
		}
		if req.Playback.PreProbeAfterScan != nil {
			s.Cfg.Playback.PreProbeAfterScan = *req.Playback.PreProbeAfterScan
		}
		if req.Playback.PreProbeMaxFiles != nil && *req.Playback.PreProbeMaxFiles >= 0 {
			s.Cfg.Playback.PreProbeMaxFiles = *req.Playback.PreProbeMaxFiles
		}
		if req.Playback.PreProbeConcurrency != nil && *req.Playback.PreProbeConcurrency > 0 {
			s.Cfg.Playback.PreProbeConcurrency = *req.Playback.PreProbeConcurrency
		}
		if req.Playback.AppleHEVCCopy != nil {
			s.Cfg.Playback.AppleHEVCCopy = *req.Playback.AppleHEVCCopy
		}
		if req.Playback.HLSSessionPackager != nil {
			s.Cfg.Playback.HLSSessionPackager = *req.Playback.HLSSessionPackager
		}
		if req.Playback.HLSCacheMB != nil {
			s.Cfg.Playback.HLSCacheMB = *req.Playback.HLSCacheMB
			if s.Cfg.Playback.HLSCacheMB < 0 {
				s.Cfg.Playback.HLSCacheMB = 0
			}
		}
	}
	if req.Users != nil {
		if req.Users.Registration != nil {
			switch *req.Users.Registration {
			case config.RegistrationOpen, config.RegistrationApproval, config.RegistrationClosed:
				s.Cfg.Users.Registration = *req.Users.Registration
			default:
				// Silently keeping the old mode would leave the admin believing
				// signup was closed when it is not.
				fail(w, 400, "注册方式只能是 open / approval / closed")
				return
			}
		}
		if req.Users.MinPassword != nil && *req.Users.MinPassword > 0 {
			s.Cfg.Users.MinPassword = *req.Users.MinPassword
		}
		if req.Users.RequireEmail != nil {
			s.Cfg.Users.RequireEmail = *req.Users.RequireEmail
		}
		if req.Users.BaseURL != nil {
			s.Cfg.Users.BaseURL = strings.TrimRight(strings.TrimSpace(*req.Users.BaseURL), "/")
		}
	}
	if req.SMTP != nil {
		// Every field is optional-by-pointer so a form that only edits the host
		// cannot wipe the password it never loaded.
		if req.SMTP.Host != nil {
			s.Cfg.SMTP.Host = strings.TrimSpace(*req.SMTP.Host)
		}
		if req.SMTP.Port != nil && *req.SMTP.Port > 0 {
			s.Cfg.SMTP.Port = *req.SMTP.Port
		}
		if req.SMTP.Username != nil {
			s.Cfg.SMTP.Username = strings.TrimSpace(*req.SMTP.Username)
		}
		if req.SMTP.Password != nil {
			s.Cfg.SMTP.Password = *req.SMTP.Password
		}
		if req.SMTP.From != nil {
			s.Cfg.SMTP.From = strings.TrimSpace(*req.SMTP.From)
		}
		if req.SMTP.FromName != nil {
			s.Cfg.SMTP.FromName = strings.TrimSpace(*req.SMTP.FromName)
		}
		if req.SMTP.Encryption != nil {
			switch *req.SMTP.Encryption {
			case config.SMTPStartTLS, config.SMTPImplicitTLS, config.SMTPNone:
				s.Cfg.SMTP.Encryption = *req.SMTP.Encryption
			default:
				fail(w, 400, "加密方式只能是 starttls / tls / none")
				return
			}
		}
		if req.SMTP.Proxy != nil {
			s.Cfg.SMTP.Proxy = strings.TrimSpace(*req.SMTP.Proxy)
		}
		if req.SMTP.SkipVerify != nil {
			s.Cfg.SMTP.SkipVerify = *req.SMTP.SkipVerify
		}
	}
	if req.Turnstile != nil {
		if req.Turnstile.Enabled != nil {
			s.Cfg.Turnstile.Enabled = *req.Turnstile.Enabled
		}
		if req.Turnstile.SiteKey != nil {
			s.Cfg.Turnstile.SiteKey = strings.TrimSpace(*req.Turnstile.SiteKey)
		}
		if req.Turnstile.SecretKey != nil {
			s.Cfg.Turnstile.SecretKey = strings.TrimSpace(*req.Turnstile.SecretKey)
		}
		if req.Turnstile.UseProxy != nil {
			s.Cfg.Turnstile.UseProxy = *req.Turnstile.UseProxy
		}
		if req.Turnstile.Proxy != nil {
			s.Cfg.Turnstile.Proxy = strings.TrimSpace(*req.Turnstile.Proxy)
		}
	}
	if err := s.Cfg.Save(s.configFile()); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if s.Engine != nil {
		s.Engine.SetBins(s.Cfg.FFmpeg, s.Cfg.FFProbe)
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

type configTMDB = struct {
	APIKey   string  `json:"api_key"`
	Language string  `json:"language"`
	APIBase  string  `json:"api_base"`
	ImgBase  string  `json:"img_base"`
	UseProxy *bool   `json:"use_proxy"`
	Proxy    *string `json:"proxy"`
}

type configBGM = struct {
	Token    string  `json:"token"`
	APIBase  string  `json:"api_base"`
	UseProxy *bool   `json:"use_proxy"`
	Proxy    *string `json:"proxy"`
}

type configPlayback = struct {
	ForceMode       string  `json:"force_mode"`
	SegmentSeconds  float64 `json:"segment_seconds"`
	TranscodeHeight int     `json:"transcode_height"`
	// pointers so "not sent" is distinguishable from an explicit false/0
	ProbeSizeMB         *int  `json:"probe_size_mb"`
	ProbeAnalyzeSec     *int  `json:"probe_analyze_sec"`
	PreProbeAfterScan   *bool `json:"preprobe_after_scan"`
	PreProbeMaxFiles    *int  `json:"preprobe_max_files"`
	PreProbeConcurrency *int  `json:"preprobe_concurrency"`
	AppleHEVCCopy       *bool `json:"apple_hevc_copy"`
	HLSSessionPackager  *bool `json:"hls_session_packager"`
	HLSCacheMB          *int  `json:"hls_cache_mb"`
}

type configUsers = struct {
	Registration *string `json:"registration"`
	MinPassword  *int    `json:"min_password"`
	RequireEmail *bool   `json:"require_email"`
	BaseURL      *string `json:"base_url"`
}

type configSMTP = struct {
	Host       *string `json:"host"`
	Port       *int    `json:"port"`
	Username   *string `json:"username"`
	Password   *string `json:"password"`
	From       *string `json:"from"`
	FromName   *string `json:"from_name"`
	Encryption *string `json:"encryption"`
	Proxy      *string `json:"proxy"`
	SkipVerify *bool   `json:"skip_verify"`
}

type configTurnstile = struct {
	Enabled   *bool   `json:"enabled"`
	SiteKey   *string `json:"site_key"`
	SecretKey *string `json:"secret_key"`
	UseProxy  *bool   `json:"use_proxy"`
	Proxy     *string `json:"proxy"`
}
