package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// DefaultTMDBProxy is the usual local SOCKS5 port (Clash/v2rayN default).
const DefaultTMDBProxy = "socks5://127.0.0.1:7890"

// Registration modes for self-service signup.
const (
	// RegistrationOpen lets a visitor register and sign in immediately.
	RegistrationOpen = "open"
	// RegistrationApproval creates the account as pending; an admin approves it
	// from the settings page before it can sign in.
	RegistrationApproval = "approval"
	// RegistrationClosed hides signup entirely: only an admin creates accounts.
	RegistrationClosed = "closed"
)

// DefaultMinPassword matches the floor the first-run setup form has always
// enforced.
const DefaultMinPassword = 4

// SMTP encryption modes.
const (
	// SMTPStartTLS connects in the clear and upgrades on port 587 (the common
	// submission port). The default.
	SMTPStartTLS = "starttls"
	// SMTPImplicitTLS wraps the connection in TLS from the first byte (port 465).
	SMTPImplicitTLS = "tls"
	// SMTPNone suits a local relay on the same machine.
	SMTPNone = "none"
)

// SMTP is the outbound mail server used for address verification and password
// resets. With no host configured the mail features stay off: registration
// never asks for an address and the settings page says so.
type SMTP struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	// Password lives in config.json in clear text. That file is already this
	// server's trust boundary (it holds the session secret), so a second secret
	// store would only add one more place to leak.
	Password string `json:"password"`
	// From is the envelope sender; it defaults to Username, because most
	// providers reject a From that is not the authenticated mailbox.
	From     string `json:"from"`
	FromName string `json:"from_name"`
	// Encryption is one of SMTPStartTLS / SMTPImplicitTLS / SMTPNone.
	Encryption string `json:"encryption"`
	// Proxy routes the SMTP connection through socks5:// or http://. Gmail's
	// SMTP is unreachable from mainland China without one.
	Proxy string `json:"proxy"`
	// SkipVerify disables certificate checks, for a self-signed local relay.
	SkipVerify bool `json:"skip_verify"`
}

// Turnstile is Cloudflare's CAPTCHA widget, used on the sign-in and signup
// forms. Both keys are required before it is enforced.
type Turnstile struct {
	Enabled   bool   `json:"enabled"`
	SiteKey   string `json:"site_key"`
	SecretKey string `json:"secret_key"`
	// UseProxy routes the server-side siteverify call. The widget itself is
	// loaded by the visitor's browser, which must be able to reach
	// challenges.cloudflare.com.
	UseProxy bool   `json:"use_proxy"`
	Proxy    string `json:"proxy"`
}

// Users controls who may get an account on this server, and how strong their
// passwords have to be.
type Users struct {
	// Registration is one of RegistrationOpen / RegistrationApproval /
	// RegistrationClosed.
	Registration string `json:"registration"`
	// MinPassword is the shortest password accepted at signup, when an admin
	// creates an account, and when a password is changed.
	MinPassword int `json:"min_password"`
	// RequireEmail makes a self-registration prove it owns an address before it
	// can sign in. It needs a working SMTP server, so it is ignored — and the
	// settings page says so — while none is configured.
	RequireEmail bool `json:"require_email"`
	// BaseURL is the address this server is reached at, used to build the links
	// in outgoing mail. Left empty, the link is built from the request's Host,
	// which is right for a plain LAN address and wrong behind a proxy.
	BaseURL string `json:"base_url"`
}

// MailEnabled reports whether outbound mail can actually be sent.
func (c *Config) MailEnabled() bool { return strings.TrimSpace(c.SMTP.Host) != "" }

// EmailVerificationRequired reports whether a new signup has to prove it owns
// its address. The mail server check matters: asking for a verification that
// can never arrive would leave the account unusable and the owner locked out.
func (c *Config) EmailVerificationRequired() bool {
	return c.Users.RequireEmail && c.MailEnabled()
}

// TurnstileEnabled reports whether requests must carry a valid CAPTCHA token.
// A half-filled configuration (enabled but missing a key) counts as off, so a
// typo can never lock everybody out of the sign-in page.
func (c *Config) TurnstileEnabled() bool {
	return c.Turnstile.Enabled && strings.TrimSpace(c.Turnstile.SiteKey) != "" && strings.TrimSpace(c.Turnstile.SecretKey) != ""
}

type TMDB struct {
	APIKey   string `json:"api_key"`
	Language string `json:"language"`
	APIBase  string `json:"api_base"`
	ImgBase  string `json:"img_base"`
	// UseProxy routes TMDB API calls and poster/backdrop downloads through a
	// local proxy — required for api.themoviedb.org / image.tmdb.org from
	// mainland China.
	UseProxy bool   `json:"use_proxy"`
	Proxy    string `json:"proxy"`
}

// BGMTV is the Bangumi (bgm.tv) metadata source — the best Chinese-language
// source for anime titles, episode titles and air dates.
type BGMTV struct {
	// Token is an optional personal access token (next.bgm.tv/demo/access-token).
	// Public search/detail/episode endpoints work without it.
	Token   string `json:"token"`
	APIBase string `json:"api_base"`
	// UseProxy defaults to true: api.bgm.tv is frequently DNS-poisoned from
	// mainland China, so a local proxy is usually the only way to reach it.
	UseProxy bool   `json:"use_proxy"`
	Proxy    string `json:"proxy"`
}

type Playback struct {
	ForceMode      string  `json:"force_mode"` // auto | direct | proxy | remux
	SegmentSeconds float64 `json:"segment_seconds"`
	// TranscodeHeight now only bounds a burned-in subtitle's output: the one
	// remaining path that re-encodes video. Ordinary playback never re-encodes.
	TranscodeHeight int `json:"transcode_height"`

	// ProbeSizeMB / ProbeAnalyzeSec bound how much of the source ffprobe pulls
	// before playback can be decided. The historical fixed 50MB/20s values cost
	// roughly 60MB per file, which on a cloud drive meant minutes of waiting
	// before the decision even came back (and every first play paid it again).
	// A small window resolves container/codecs for virtually all real files;
	// the deep values are still used as an automatic fallback when it doesn't.
	ProbeSizeMB     int `json:"probe_size_mb"`
	ProbeAnalyzeSec int `json:"probe_analyze_sec"`

	// PreProbeAfterScan warms the probe cache in the background after a library
	// scan, so opening a file is already decided. PreProbeMaxFiles caps how many
	// files one scan warms (0 = unlimited); PreProbeConcurrency bounds parallel
	// probes so the cloud drive is not hammered.
	PreProbeAfterScan   bool `json:"preprobe_after_scan"`
	PreProbeMaxFiles    int  `json:"preprobe_max_files"`
	PreProbeConcurrency int  `json:"preprobe_concurrency"`

	// AppleHEVCCopy lets Apple clients copy HEVC into MPEG-TS instead of
	// transcoding. It is OFF by default because Apple's HLS stack does not decode
	// HEVC carried in MPEG-TS: such a copy plays audio with no picture. Enable it
	// only on a device that has been verified to play HEVC-in-TS.
	AppleHEVCCopy bool `json:"apple_hevc_copy"`

	// HLSSessionPackager routes HEVC copy for Apple through one long-lived ffmpeg
	// that packages fMP4 HLS — the only form of copied HEVC Apple will play — so
	// copied HEVC needs no transcoding at all. It writes the packaged segments
	// under <data>/cache/hls for the life of a playback session, trading
	// temporary disk space for CPU. Off by default.
	HLSSessionPackager bool `json:"hls_session_packager"`

	// HLSCacheMB bounds the packaged-segment cache under <data>/cache/hls. A
	// session that finished the whole file is kept, so replaying that file needs
	// no packaging at all; the least recently used are evicted to stay inside this
	// budget, and nothing partial survives a restart. 0 turns the cache off, and
	// then a session's output is deleted as soon as it ends.
	HLSCacheMB int `json:"hls_cache_mb"`
}

type Config struct {
	Port            int       `json:"port"`
	DataDir         string    `json:"data_dir"`
	Secret          string    `json:"secret"`
	FFmpeg          string    `json:"ffmpeg"`
	FFProbe         string    `json:"ffprobe"`
	TMDB            TMDB      `json:"tmdb"`
	BGM             BGMTV     `json:"bgm"`
	Playback        Playback  `json:"playback"`
	Users           Users     `json:"users"`
	SMTP            SMTP      `json:"smtp"`
	Turnstile       Turnstile `json:"turnstile"`
	ScanConcurrency int       `json:"scan_concurrency"`
	DevWebDir       string    `json:"dev_web_dir"`
	// ExcludeKeywords is what a scan refuses to import: release furniture
	// (NCED, menus, previews…) and anything else the user wants kept out of the
	// library. A library that keeps its own list replaces this one.
	ExcludeKeywords []string `json:"exclude_keywords"`
}

// DefaultExcludeKeywords is the list a fresh install starts with. It is written
// into the config file rather than applied on load: a user who deletes an entry
// has deleted it, and a user who wants NCED back must be able to remove it.
func DefaultExcludeKeywords() []string {
	return []string{"ncop", "nced", "nc-op", "nc-ed", "creditless", "menu", "preview", "trailer", "sample", "pv", "cm", "sp"}
}

// RegistrationAllowed reports whether visitors may create their own account,
// either immediately or pending an admin's approval.
func (c *Config) RegistrationAllowed() bool {
	return c.Users.Registration == RegistrationOpen || c.Users.Registration == RegistrationApproval
}

func Defaults() *Config {
	return &Config{
		Port:    5666,
		DataDir: "data",
		Secret:  randomSecret(),
		FFmpeg:  "ffmpeg",
		FFProbe: "ffprobe",
		TMDB: TMDB{
			Language: "zh-CN",
			APIBase:  "https://api.themoviedb.org/3",
			ImgBase:  "https://image.tmdb.org/t/p",
			Proxy:    DefaultTMDBProxy,
		},
		BGM: BGMTV{
			APIBase:  "https://api.bgm.tv",
			UseProxy: true,
			Proxy:    DefaultTMDBProxy,
		},
		Playback: Playback{
			ForceMode:           "auto",
			SegmentSeconds:      4,
			TranscodeHeight:     1080,
			ProbeSizeMB:         8,
			ProbeAnalyzeSec:     4,
			PreProbeAfterScan:   true,
			PreProbeMaxFiles:    100,
			PreProbeConcurrency: 1,
			HLSCacheMB:          2048,
		},
		Users: Users{
			Registration: RegistrationOpen,
			MinPassword:  DefaultMinPassword,
		},
		SMTP: SMTP{
			Port:       587,
			Encryption: SMTPStartTLS,
			FromName:   "DoriyaMedia",
		},
		Turnstile: Turnstile{
			UseProxy: true,
			Proxy:    DefaultTMDBProxy,
		},
		ScanConcurrency: 4,
		ExcludeKeywords: DefaultExcludeKeywords(),
	}
}

func Load(path string) (*Config, error) {
	cfg := Defaults()
	raw, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	cfg.normalize()
	if err := cfg.Save(path); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) normalize() {
	if c.Port <= 0 {
		c.Port = 5666
	}
	if c.DataDir == "" {
		c.DataDir = "data"
	}
	if c.Secret == "" {
		c.Secret = randomSecret()
	}
	if c.TMDB.Language == "" {
		c.TMDB.Language = "zh-CN"
	}
	if c.TMDB.APIBase == "" {
		c.TMDB.APIBase = "https://api.themoviedb.org/3"
	}
	if c.TMDB.ImgBase == "" {
		c.TMDB.ImgBase = "https://image.tmdb.org/t/p"
	}
	if c.TMDB.Proxy == "" {
		c.TMDB.Proxy = DefaultTMDBProxy
	}
	if c.BGM.APIBase == "" {
		c.BGM.APIBase = "https://api.bgm.tv"
	}
	if c.BGM.Proxy == "" {
		c.BGM.Proxy = DefaultTMDBProxy
	}
	if c.Playback.ForceMode == "" {
		c.Playback.ForceMode = "auto"
	}
	if c.Playback.SegmentSeconds <= 0 {
		c.Playback.SegmentSeconds = 4
	}
	if c.Playback.TranscodeHeight <= 0 {
		c.Playback.TranscodeHeight = 1080
	}
	if c.Playback.ProbeSizeMB <= 0 {
		c.Playback.ProbeSizeMB = 8
	}
	if c.Playback.ProbeSizeMB > 512 {
		c.Playback.ProbeSizeMB = 512
	}
	if c.Playback.ProbeAnalyzeSec <= 0 {
		c.Playback.ProbeAnalyzeSec = 4
	}
	if c.Playback.ProbeAnalyzeSec > 120 {
		c.Playback.ProbeAnalyzeSec = 120
	}
	if c.Playback.PreProbeMaxFiles < 0 {
		c.Playback.PreProbeMaxFiles = 0
	}
	if c.Playback.PreProbeConcurrency <= 0 {
		c.Playback.PreProbeConcurrency = 1
	}
	if c.Playback.PreProbeConcurrency > 8 {
		c.Playback.PreProbeConcurrency = 8
	}
	// The cache budget is in MB; an unset value gets the default rather than
	// silently disabling the cache, and a negative one means "off".
	if c.Playback.HLSCacheMB < 0 {
		c.Playback.HLSCacheMB = 0
	}
	if c.Playback.HLSCacheMB > 102400 {
		c.Playback.HLSCacheMB = 102400
	}
	if c.ScanConcurrency <= 0 {
		c.ScanConcurrency = 4
	}
	// An unknown registration mode would silently lock people out of a server
	// whose owner meant to leave signup on, so it falls back to the default.
	switch c.Users.Registration {
	case RegistrationOpen, RegistrationApproval, RegistrationClosed:
	default:
		c.Users.Registration = RegistrationOpen
	}
	if c.Users.MinPassword < DefaultMinPassword {
		c.Users.MinPassword = DefaultMinPassword
	}
	if c.Users.MinPassword > 128 {
		c.Users.MinPassword = 128
	}
	// A link in an email has to be absolute, so a base URL that ends in a slash
	// would produce "//verify-email" — harmless in a browser, ugly in a mail
	// client, and easy to prevent here.
	c.Users.BaseURL = strings.TrimRight(strings.TrimSpace(c.Users.BaseURL), "/")

	if strings.TrimSpace(c.SMTP.Host) != "" {
		c.SMTP.Host = strings.TrimSpace(c.SMTP.Host)
		switch c.SMTP.Encryption {
		case SMTPStartTLS, SMTPImplicitTLS, SMTPNone:
		default:
			c.SMTP.Encryption = SMTPStartTLS
		}
		if c.SMTP.Port <= 0 || c.SMTP.Port > 65535 {
			// The usual port for the chosen mode, so filling in only the host
			// still works.
			if c.SMTP.Encryption == SMTPImplicitTLS {
				c.SMTP.Port = 465
			} else {
				c.SMTP.Port = 587
			}
		}
		if strings.TrimSpace(c.SMTP.From) == "" {
			c.SMTP.From = c.SMTP.Username
		}
		if strings.TrimSpace(c.SMTP.FromName) == "" {
			c.SMTP.FromName = "DoriyaMedia"
		}
	}
	if c.Turnstile.Proxy == "" {
		c.Turnstile.Proxy = DefaultTMDBProxy
	}
	if c.FFmpeg == "" {
		c.FFmpeg = "ffmpeg"
	}
	if c.FFProbe == "" {
		c.FFProbe = "ffprobe"
	}
}

func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// config.json contains the session secret, SMTP password and API keys. The
	// owner needs read/write; no other local account needs either. The chmod is
	// also applied to an existing file, because WriteFile only uses the mode
	// when it creates one.
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func (c *Config) Abs() string {
	if filepath.IsAbs(c.DataDir) {
		return c.DataDir
	}
	wd, _ := os.Getwd()
	return filepath.Join(wd, c.DataDir)
}

func (c *Config) DBPath() string    { return filepath.Join(c.Abs(), "doriyamedia.db") }
func (c *Config) SitePath() string  { return filepath.Join(c.Abs(), "site.json") }
func (c *Config) ImagesDir() string { return filepath.Join(c.Abs(), "images") }
func (c *Config) LogsDir() string   { return filepath.Join(c.Abs(), "logs") }

func randomSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
