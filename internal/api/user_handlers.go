package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"doriyamedia/internal/auth"
	"doriyamedia/internal/config"
	"doriyamedia/internal/db"
	"doriyamedia/internal/models"
)

const (
	// sessionTTL is how long one sign-in lasts before the cookie expires.
	sessionTTL = 30 * 24 * time.Hour

	minNameLen = 2
	maxNameLen = 32
	// maxPasswordLen keeps a huge body from turning into a minutes-long PBKDF2
	// derivation on the server.
	maxPasswordLen = 200
)

// nameRe keeps account names to something readable in the UI and harmless in a
// log line: letters in any script (so Chinese names work), digits, and _ - . @
var nameRe = regexp.MustCompile(`^[\p{L}\p{N}_.@-]+$`)

// validateName trims and checks a requested account name.
func validateName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	switch n := utf8.RuneCountInString(name); {
	case n < minNameLen || n > maxNameLen:
		return "", fmt.Errorf("用户名需要 %d-%d 个字符", minNameLen, maxNameLen)
	case !nameRe.MatchString(name):
		return "", fmt.Errorf("用户名只能包含中英文、数字与 _ - . @")
	}
	return name, nil
}

// passwordMin is the effective minimum password length.
func (s *Server) passwordMin() int {
	min := s.Cfg.Users.MinPassword
	if min < config.DefaultMinPassword {
		min = config.DefaultMinPassword
	}
	return min
}

func (s *Server) validatePassword(pw string) error {
	if utf8.RuneCountInString(pw) < s.passwordMin() {
		return fmt.Errorf("密码至少 %d 位", s.passwordMin())
	}
	if len(pw) > maxPasswordLen {
		return fmt.Errorf("密码过长(最多 %d 字节)", maxPasswordLen)
	}
	return nil
}

// clientIP is the address a request came from. X-Forwarded-For is deliberately
// ignored: a client can set that header itself, and trusting it would turn the
// sign-in throttle into a no-op.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// normalizeRole maps any input onto the two roles that exist.
func normalizeRole(role string) string {
	if role == models.RoleAdmin {
		return models.RoleAdmin
	}
	return models.RoleUser
}

// normalizeStatus maps any input onto a known account state.
func normalizeStatus(status string) string {
	switch status {
	case models.StatusDisabled, models.StatusPending:
		return status
	default:
		return models.StatusActive
	}
}

// ---------- registration (public) ----------

// handleRegister creates an account from the signup form. Whether that account
// can sign in immediately, must wait for an admin, or may not be created at all
// is the admin's registration policy.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name           string `json:"name"`
		Password       string `json:"password"`
		Email          string `json:"email"`
		TurnstileToken string `json:"turnstile_token"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	// A server with no account yet is in first-run setup. Letting a stranger
	// register here would either hand them the server or, by making the user
	// count non-zero, lock the real owner out of the setup flow.
	n, err := s.Store.UserCount()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if n == 0 {
		fail(w, 400, "服务尚未初始化,请先创建管理员账号")
		return
	}
	if !s.Cfg.RegistrationAllowed() {
		fail(w, 403, "管理员已关闭注册")
		return
	}
	// Asking for a verification that can never arrive would create an account
	// nobody can use, so the missing mail server is reported before anything is
	// written.
	if s.Cfg.Users.RequireEmail && !s.Cfg.MailEnabled() {
		fail(w, 503, "管理员开启了邮箱验证,但服务器还没有配置邮件发送,请联系管理员")
		return
	}
	key := clientIP(r)
	if !s.registerLimiter.Allow(key) {
		fail(w, 429, "注册过于频繁,请稍后再试")
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
	taken, err := s.Store.NameTaken(name, 0)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if taken {
		fail(w, 409, "这个用户名已被使用")
		return
	}

	needEmail := s.Cfg.EmailVerificationRequired()
	var email string
	if needEmail {
		email, err = normalizeEmail(req.Email)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		used, err := s.Store.EmailTaken(email, 0)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		if used {
			fail(w, 409, "这个邮箱已被其他账号使用")
			return
		}
	}

	// The challenge is checked last, so fixing a typo in the form does not cost
	// the visitor a fresh one.
	if err := s.verifyTurnstile(r, req.TurnstileToken); err != nil {
		s.registerLimiter.Hit(key)
		fail(w, 400, err.Error())
		return
	}

	u := &models.User{Name: name, Role: models.RoleUser, Status: models.StatusActive, Email: email}
	if s.Cfg.Users.Registration == config.RegistrationApproval {
		u.Status = models.StatusPending
	}
	u.PassHash, u.Salt = auth.HashPassword(req.Password)
	if err := s.Store.CreateUser(u); err != nil {
		fail(w, 500, err.Error())
		return
	}
	s.registerLimiter.Hit(key)

	if needEmail {
		// The account exists either way; only the mail can fail, and then the
		// person can ask for another link once the admin has fixed SMTP.
		body := map[string]any{
			"created": true,
			"pending": u.Status == models.StatusPending,
		}
		if err := s.sendVerification(r, u); err != nil {
			body["verify_email_sent"] = false
			body["message"] = "账号已创建,但验证邮件发送失败: " + err.Error() + "。修好后可在登录页点「重新发送验证邮件」。"
		} else {
			body["verify_email_sent"] = true
			msg := "验证邮件已发送到 " + u.Email + ",请点邮件里的链接完成验证"
			if u.Status == models.StatusPending {
				msg += ";验证之后还需要管理员审核通过"
			}
			body["message"] = msg
		}
		writeJSON(w, 202, body)
		return
	}

	if u.Status == models.StatusPending {
		writeJSON(w, 202, map[string]any{
			"pending": true,
			"message": "注册成功,等待管理员审核通过后即可登录",
		})
		return
	}
	s.startSession(w, u)
}

// ---------- self service ----------

// handleChangePassword changes the caller's own password after checking the old
// one, and signs every other device out.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request, user *models.User) {
	var req struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	if !auth.Verify(req.Current, user.PassHash, user.Salt) {
		fail(w, 403, "当前密码不正确")
		return
	}
	if err := s.validatePassword(req.New); err != nil {
		fail(w, 400, err.Error())
		return
	}
	if req.New == req.Current {
		fail(w, 400, "新密码与当前密码相同")
		return
	}
	hash, salt := auth.HashPassword(req.New)
	if _, err := s.Store.RotateUserPassword(user.ID, hash, salt, time.Now(), db.PasswordRotationOptions{
		PreserveSessionToken: sessionToken(r),
	}); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleUpdateProfile renames the caller's own account.
func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request, user *models.User) {
	var req struct {
		Name string `json:"name"`
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
	taken, err := s.Store.NameTaken(name, user.ID)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if taken {
		fail(w, 409, "这个用户名已被使用")
		return
	}
	if err := s.Store.RenameUser(user.ID, name); err != nil {
		fail(w, 500, err.Error())
		return
	}
	user.Name = name
	writeJSON(w, 200, map[string]any{"user": user})
}

// ---------- admin: user management ----------

// handleListUsers returns every account for the settings page. Password hashes
// and salts never leave the server (the model tags them json:"-").
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request, user *models.User) {
	users, err := s.Store.ListUsers()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if users == nil {
		users = []models.User{}
	}
	writeJSON(w, 200, map[string]any{"users": users})
}

// handleCreateUser is the admin-side "add account", for when registration is
// closed or someone should be let in without a signup round trip.
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request, user *models.User) {
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
		Role     string `json:"role"`
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
	taken, err := s.Store.NameTaken(name, 0)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if taken {
		fail(w, 409, "这个用户名已被使用")
		return
	}
	u := &models.User{Name: name, Role: normalizeRole(req.Role), Status: models.StatusActive}
	u.PassHash, u.Salt = auth.HashPassword(req.Password)
	if err := s.Store.CreateUser(u); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"user": u})
}

// handleUpdateUser edits one account: its name, its role, its state, or any
// combination. Approving a pending signup is a status change to active.
func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request, actor *models.User) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "无效的用户 ID")
		return
	}
	target, err := s.Store.UserByID(id)
	if err != nil {
		fail(w, 404, "用户不存在")
		return
	}
	var req struct {
		Name          *string `json:"name"`
		Role          *string `json:"role"`
		Status        *string `json:"status"`
		Email         *string `json:"email"`
		EmailVerified *bool   `json:"email_verified"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}

	var update db.AdminUserUpdate
	update.ActorID = actor.ID
	update.UserID = target.ID
	var role, status string
	role, status = target.Role, target.Status
	if req.Role != nil {
		role = normalizeRole(*req.Role)
		update.Role = &role
	}
	if req.Status != nil {
		status = normalizeStatus(*req.Status)
		update.Status = &status
	}

	// Disabling yourself would take the panel away mid-session and cannot be
	// undone from where you are standing. Stepping down from admin is allowed
	// though, as long as someone else can still administer the server.
	if target.ID == actor.ID {
		if req.Status != nil && status != target.Status {
			fail(w, 400, "不能禁用自己的账号")
			return
		}
	}

	if req.Name != nil {
		name, err := validateName(*req.Name)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		update.Name = &name
	}
	// An admin can correct an address or vouch for one by hand, which is the way
	// out for someone whose mail never arrived.
	if req.Email != nil || req.EmailVerified != nil {
		if req.Email != nil {
			// An empty address is allowed here: it removes the record and with
			// it any pending verification.
			email := ""
			if strings.TrimSpace(*req.Email) == "" {
				update.Email = &email
			} else {
				normalized, err := normalizeEmail(*req.Email)
				if err != nil {
					fail(w, 400, err.Error())
					return
				}
				email = normalized
				update.Email = &email
			}
		}
		update.EmailVerified = req.EmailVerified
	}

	updated, err := s.Store.UpdateUserAdmin(update, time.Now())
	if err != nil {
		failAdminUserMutation(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"user": updated})
}

// handleDeleteUser removes an account along with its sessions and playback
// progress.
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request, actor *models.User) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "无效的用户 ID")
		return
	}
	target, err := s.Store.UserByID(id)
	if err != nil {
		fail(w, 404, "用户不存在")
		return
	}
	if target.ID == actor.ID {
		fail(w, 400, "不能删除当前登录的账号")
		return
	}
	if err := s.Store.DeleteUserAdmin(actor.ID, target.ID, time.Now()); err != nil {
		failAdminUserMutation(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleSetUserPassword lets an admin set a new password for someone who forgot
// theirs, and signs that account's other devices out.
func (s *Server) handleSetUserPassword(w http.ResponseWriter, r *http.Request, actor *models.User) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "无效的用户 ID")
		return
	}
	target, err := s.Store.UserByID(id)
	if err != nil {
		fail(w, 404, "用户不存在")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	if err := s.validatePassword(req.Password); err != nil {
		fail(w, 400, err.Error())
		return
	}
	hash, salt := auth.HashPassword(req.Password)
	preserve := ""
	if target.ID == actor.ID {
		preserve = sessionToken(r)
	}
	if _, err := s.Store.RotateUserPassword(target.ID, hash, salt, time.Now(), db.PasswordRotationOptions{
		PreserveSessionToken: preserve,
	}); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleRevokeUserSessions signs an account out everywhere: the "踢下线" button.
func (s *Server) handleRevokeUserSessions(w http.ResponseWriter, r *http.Request, actor *models.User) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "无效的用户 ID")
		return
	}
	if _, err := s.Store.UserByID(id); err != nil {
		fail(w, 404, "用户不存在")
		return
	}
	exclude := ""
	if id == actor.ID {
		exclude = sessionToken(r)
	}
	if _, err := s.Store.RevokeUserSessionsAndDevices(id, exclude, time.Now()); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func failAdminUserMutation(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, db.ErrUserNotFound):
		fail(w, 404, "用户不存在")
	case errors.Is(err, db.ErrAdminActorUnavailable):
		fail(w, 403, "需要管理员权限")
	case errors.Is(err, db.ErrAdminCannotChangeOwnState):
		fail(w, 400, "不能禁用自己的账号")
	case errors.Is(err, db.ErrAdminCannotDeleteSelf):
		fail(w, 400, "不能删除当前登录的账号")
	case errors.Is(err, db.ErrLastActiveAdmin):
		fail(w, 400, "至少要保留一个可用的管理员账号")
	case errors.Is(err, db.ErrUserNameTaken):
		fail(w, 409, "这个用户名已被使用")
	case errors.Is(err, db.ErrUserEmailTaken):
		fail(w, 409, "这个邮箱已被其他账号使用")
	default:
		fail(w, 500, err.Error())
	}
}
