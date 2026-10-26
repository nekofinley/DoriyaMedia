package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"doriyamedia/internal/auth"
	"doriyamedia/internal/db"
	"doriyamedia/internal/mail"
	"doriyamedia/internal/models"
)

const (
	// verifyTokenTTL is generous: people read mail on their phone, hours later.
	verifyTokenTTL = 24 * time.Hour
	// resetTokenTTL is deliberately short — a reset link is a live key to the
	// account, and an hour is plenty to click it.
	resetTokenTTL = time.Hour

	maxEmailLen = 128
)

// emailRe is deliberately loose: the only authority on whether an address
// exists is the mail server, and a strict pattern mostly rejects valid oddities
// (plus-addressing, long TLDs, IDN).
var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s.]+(\.[^@\s.]+)+$`)

// normalizeEmail trims, checks and lowercases an address. Lowercasing is what
// makes "Foo@x.com" and "foo@x.com" the same account rather than two.
func normalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if email == "" {
		return "", errors.New("请填写邮箱地址")
	}
	if len(email) > maxEmailLen {
		return "", errors.New("邮箱地址过长")
	}
	if !emailRe.MatchString(email) {
		return "", errors.New("邮箱地址格式不正确")
	}
	return email, nil
}

// mailer builds a sender from the current configuration. Building it per send
// costs nothing (no connection is opened until Send) and picks up settings the
// admin just saved without a restart.
func (s *Server) mailer() *mail.Sender {
	return mail.New(mail.Config{
		Host:       s.Cfg.SMTP.Host,
		Port:       s.Cfg.SMTP.Port,
		Username:   s.Cfg.SMTP.Username,
		Password:   s.Cfg.SMTP.Password,
		From:       s.Cfg.SMTP.From,
		FromName:   s.Cfg.SMTP.FromName,
		Encryption: s.Cfg.SMTP.Encryption,
		Proxy:      s.Cfg.SMTP.Proxy,
		SkipVerify: s.Cfg.SMTP.SkipVerify,
	})
}

// baseURLFor is the absolute prefix for links in mail. The configured value
// wins; otherwise the address the visitor actually used is reused, which is
// right for a plain LAN install and wrong behind a proxy that rewrites Host —
// hence the setting.
func (s *Server) baseURLFor(r *http.Request) string {
	if u := strings.TrimSpace(s.Cfg.Users.BaseURL); u != "" {
		return u
	}
	scheme := "http"
	// Only ever used to build a link, never to make a trust decision, so a
	// spoofed header here cannot do more than put a wrong hostname in an email
	// the account owner is about to read.
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1:" + strconv.Itoa(s.Cfg.Port)
	}
	return scheme + "://" + host
}

// sendVerification issues a fresh token and mails the link. A new link
// invalidates the previous one, so an old message cannot be replayed.
func (s *Server) sendVerification(r *http.Request, u *models.User) error {
	if !s.Cfg.MailEnabled() {
		return errors.New("服务器还没有配置邮件发送")
	}
	if strings.TrimSpace(u.Email) == "" {
		return errors.New("这个账号还没有邮箱地址")
	}
	token := auth.NewToken()
	if err := s.Store.CreateAuthTokenForEmail(
		token, u.ID, db.TokenVerifyEmail, u.Email, time.Now().Add(verifyTokenTTL),
	); err != nil {
		return err
	}
	link := fmt.Sprintf("%s/verify-email?token=%s", s.baseURLFor(r), token)
	subject, html, text := verificationMail(u.Name, link, verifyTokenTTL)
	return s.mailer().Send(u.Email, subject, html, text)
}

func (s *Server) sendPasswordReset(r *http.Request, u *models.User) error {
	if !s.Cfg.MailEnabled() {
		return errors.New("服务器还没有配置邮件发送")
	}
	if strings.TrimSpace(u.Email) == "" {
		return errors.New("这个账号还没有邮箱地址")
	}
	token := auth.NewToken()
	if err := s.Store.CreateAuthTokenForEmail(
		token, u.ID, db.TokenResetPassword, u.Email, time.Now().Add(resetTokenTTL),
	); err != nil {
		return err
	}
	link := fmt.Sprintf("%s/reset-password?token=%s", s.baseURLFor(r), token)
	subject, html, text := resetMail(u.Name, link, resetTokenTTL)
	return s.mailer().Send(u.Email, subject, html, text)
}

// ---------- public: completing a verification ----------

// handleVerifyEmail consumes the token from the link in the mail. It reports
// whether the account can sign in now, because in approval mode verifying an
// address is only half of what stands in the way.
func (s *Server) handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := readBody(r, &req); err != nil || strings.TrimSpace(req.Token) == "" {
		fail(w, 400, "缺少验证令牌")
		return
	}
	userID, err := s.Store.ConsumeEmailVerificationToken(req.Token, time.Now())
	if errors.Is(err, db.ErrEmailVerificationTokenInvalid) {
		fail(w, 400, "这个验证链接无效,或已经被使用过了")
		return
	}
	if err != nil {
		fail(w, 500, "验证链接暂时无法处理,请稍后重试")
		return
	}
	u, err := s.Store.UserByID(userID)
	if err != nil {
		fail(w, 400, "这个验证链接无效,或已经被使用过了")
		return
	}

	message := "邮箱验证成功,现在可以登录了"
	switch {
	case u.Status == models.StatusPending:
		message = "邮箱验证成功,等管理员审核通过后即可登录"
	case u.Status == models.StatusDisabled:
		message = "邮箱验证成功,但这个账号已被管理员禁用"
	}
	_ = s.Store.PruneAuthTokens()
	writeJSON(w, 200, map[string]any{"ok": true, "message": message})
}

// ---------- public: resend / forgot ----------

// handleResendVerification sends a new link to an account that has not proven
// its address yet. It answers the same way whether or not the account exists,
// so the form cannot be used to enumerate accounts.
func (s *Server) handleResendVerification(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Login string `json:"login"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	key := clientIP(r)
	if !s.mailLimiter.Allow(key) {
		fail(w, 429, "发送过于频繁,请稍后再试")
		return
	}
	const answer = "如果这个账号存在且还没验证邮箱,验证邮件已经发出"
	if !s.Cfg.MailEnabled() {
		fail(w, 503, "服务器还没有配置邮件发送,请联系管理员")
		return
	}
	if u, err := s.Store.UserByLogin(strings.TrimSpace(req.Login)); err == nil && u.EmailPending() {
		s.mailLimiter.Hit(key)
		if err := s.sendVerification(r, u); err != nil {
			fail(w, 502, "邮件发送失败: "+err.Error())
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": answer})
}

// handleForgotPassword mails a reset link. Like the resend form it answers
// identically for unknown accounts, and it is rate limited so the server cannot
// be turned into a mail cannon pointed at someone else's inbox.
func (s *Server) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Login          string `json:"login"`
		TurnstileToken string `json:"turnstile_token"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	key := clientIP(r)
	if !s.mailLimiter.Allow(key) {
		fail(w, 429, "发送过于频繁,请稍后再试")
		return
	}
	if err := s.verifyTurnstile(r, req.TurnstileToken); err != nil {
		s.mailLimiter.Hit(key)
		fail(w, 400, err.Error())
		return
	}
	if !s.Cfg.MailEnabled() {
		fail(w, 503, "服务器还没有配置邮件发送,请联系管理员重置密码")
		return
	}
	login := strings.TrimSpace(req.Login)
	if login == "" {
		fail(w, 400, "请填写用户名或邮箱")
		return
	}
	if u, err := s.Store.UserByLogin(login); err == nil && u.Email != "" && u.Status != models.StatusDisabled {
		s.mailLimiter.Hit(key)
		if err := s.sendPasswordReset(r, u); err != nil {
			fail(w, 502, "邮件发送失败: "+err.Error())
			return
		}
	}
	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"message": "如果这个账号存在并且绑定了邮箱,重置密码的邮件已经发出",
	})
}

// handleResetPassword consumes a reset token and sets the new password.
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := readBody(r, &req); err != nil || strings.TrimSpace(req.Token) == "" {
		fail(w, 400, "缺少重置令牌")
		return
	}
	if err := s.validatePassword(req.Password); err != nil {
		fail(w, 400, err.Error())
		return
	}
	userID, expires, err := s.Store.AuthTokenUserID(req.Token, db.TokenResetPassword)
	if err != nil {
		fail(w, 400, "这个重置链接无效,或已经被使用过了")
		return
	}
	if time.Now().After(expires) {
		fail(w, 400, "重置链接已过期,请重新申请一封")
		return
	}
	u, err := s.Store.UserByID(userID)
	if err != nil {
		fail(w, 400, "这个重置链接无效,或已经被使用过了")
		return
	}
	hash, salt := auth.HashPassword(req.Password)
	if _, err := s.Store.RotateUserPassword(u.ID, hash, salt, time.Now(), db.PasswordRotationOptions{
		ResetToken:         req.Token,
		VerifyCurrentEmail: true,
	}); err != nil {
		if errors.Is(err, db.ErrPasswordResetTokenInvalid) || errors.Is(err, db.ErrUserNotFound) {
			fail(w, 400, "这个重置链接无效,或已经被使用过了")
			return
		}
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": "密码已重设,请用新密码登录"})
}

// ---------- signed in: bind or change your own address ----------

// handleSetOwnEmail lets a signed-in user add or change their address. The new
// address starts unverified and gets a link; until it is proven, sign-in is
// unaffected (the gate only ever looks at the current address).
func (s *Server) handleSetOwnEmail(w http.ResponseWriter, r *http.Request, user *models.User) {
	var req struct {
		Email string `json:"email"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	if !s.Cfg.MailEnabled() {
		fail(w, 503, "服务器还没有配置邮件发送")
		return
	}
	key := clientIP(r)
	if !s.mailLimiter.Allow(key) {
		fail(w, 429, "操作过于频繁,请稍后再试")
		return
	}
	email, err := normalizeEmail(req.Email)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	taken, err := s.Store.EmailTaken(email, user.ID)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if taken {
		fail(w, 409, "这个邮箱已被其他账号使用")
		return
	}
	// Changing the address clears the flag: the new one has proved nothing.
	verified := email == user.Email && user.EmailVerified
	if err := s.Store.SetUserEmail(user.ID, email, verified); err != nil {
		fail(w, 500, err.Error())
		return
	}
	user.Email, user.EmailVerified = email, verified
	s.mailLimiter.Hit(key)
	if !verified {
		updated := *user
		if err := s.sendVerification(r, &updated); err != nil {
			writeJSON(w, 200, map[string]any{
				"user":  user,
				"ok":    false,
				"error": "邮箱已保存,但验证邮件发送失败: " + err.Error(),
			})
			return
		}
	}
	writeJSON(w, 200, map[string]any{"user": user, "ok": true, "message": "验证邮件已发送,请到邮箱里点确认"})
}

// handleResendOwnVerification re-sends the link for the caller's own address.
func (s *Server) handleResendOwnVerification(w http.ResponseWriter, r *http.Request, user *models.User) {
	if !user.EmailPending() {
		fail(w, 400, "这个账号的邮箱已经验证过了")
		return
	}
	key := clientIP(r)
	if !s.mailLimiter.Allow(key) {
		fail(w, 429, "发送过于频繁,请稍后再试")
		return
	}
	s.mailLimiter.Hit(key)
	if err := s.sendVerification(r, user); err != nil {
		fail(w, 502, "邮件发送失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": "验证邮件已重新发送"})
}

// ---------- admin: prove the SMTP settings work ----------

// handleMailTest sends one message so the admin finds out here, rather than
// from a user who never received a verification link.
func (s *Server) handleMailTest(w http.ResponseWriter, r *http.Request, user *models.User) {
	var req struct {
		To string `json:"to"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	to := strings.TrimSpace(req.To)
	if to == "" {
		to = user.Email
	}
	if to == "" {
		fail(w, 400, "请填写收件邮箱")
		return
	}
	if _, err := normalizeEmail(to); err != nil {
		fail(w, 400, err.Error())
		return
	}
	sender := s.mailer()
	if !sender.Enabled() {
		fail(w, 400, "请先填写 SMTP 服务器地址")
		return
	}
	subject, html, text := testMail(s.baseURLFor(r))
	if err := sender.Send(to, subject, html, text); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": "测试邮件已发送到 " + to + ",请查收(也看一下垃圾箱)"})
}
