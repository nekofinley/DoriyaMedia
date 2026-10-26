package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"doriyamedia/internal/config"
	"doriyamedia/internal/mailtest"
)

// configureSMTP points the server at a fake mail server and returns it.
func configureSMTP(t *testing.T, srv *Server) *mailtest.Server {
	t.Helper()
	fake := mailtest.New(t)
	host, port := fake.HostPort(t)
	srv.Cfg.SMTP = config.SMTP{
		Host: host, Port: port, Encryption: config.SMTPNone,
		From: mailtest.Username, FromName: "DoriyaMedia",
	}
	return fake
}

// tokenFromLink pulls the token out of a link the mail carried.
func tokenFromLink(t *testing.T, link, prefix string) string {
	t.Helper()
	if !strings.HasPrefix(link, prefix) {
		t.Fatalf("link %q does not start with %q", link, prefix)
	}
	return strings.TrimPrefix(link, prefix)
}

// Registering with email verification on: the account is created, the mail goes
// out, sign-in is refused until the link is followed, and then it works.
func TestEmailVerificationFlow(t *testing.T) {
	srv, ts := newTestServer(t)
	base := ts.URL
	setupAdmin(t, base)
	fake := configureSMTP(t, srv)
	srv.Cfg.Users.RequireEmail = true

	if code, body := call(t, browser(t), "GET", base+"/api/bootstrap", nil); code != 200 ||
		body["require_email"] != true || body["can_register"] != true {
		t.Fatalf("bootstrap while verification is on: %v", body)
	}

	alice := browser(t)
	code, body := call(t, alice, "POST", base+"/api/auth/register", map[string]string{
		"name": "alice", "password": "alice-pass", "email": "Alice@Example.com",
	})
	if code != 202 {
		t.Fatalf("register: code %d body %v", code, body)
	}
	if body["verify_email_sent"] != true {
		t.Fatalf("verification mail was not reported as sent: %v", body)
	}
	if fake.Sent() != 1 {
		t.Fatalf("messages delivered = %d, want 1", fake.Sent())
	}
	msg := fake.Last(t)
	if msg.To != "alice@example.com" {
		t.Errorf("mail went to %q, want the lowercased address", msg.To)
	}
	if !strings.Contains(msg.Subject, "验证") {
		t.Errorf("subject does not say what it is: %q", msg.Subject)
	}

	// Not signed in, and sign-in is refused with a reason the owner can act on.
	if code, _ := call(t, alice, "GET", base+"/api/auth/me", nil); code != 401 {
		t.Errorf("an unverified signup was signed in (%d)", code)
	}
	if code, body := call(t, browser(t), "POST", base+"/api/auth/login", map[string]string{
		"name": "alice", "password": "alice-pass",
	}); code != 403 || !strings.Contains(body["error"].(string), "邮箱") {
		t.Errorf("unverified login = %d %v, want 403 mentioning the mailbox", code, body)
	}

	// Follow the link from the mail.
	token := tokenFromLink(t, fake.LinkWithPrefix(t, base+"/verify-email?token="), base+"/verify-email?token=")
	code, body = call(t, browser(t), "POST", base+"/api/auth/verify-email", map[string]string{"token": token})
	if code != 200 {
		t.Fatalf("verify: code %d body %v", code, body)
	}
	// The token is one-shot.
	if code, _ := call(t, browser(t), "POST", base+"/api/auth/verify-email", map[string]string{"token": token}); code != 400 {
		t.Errorf("a verification token was accepted twice (%d)", code)
	}
	if code, _ := call(t, browser(t), "POST", base+"/api/auth/login", map[string]string{
		"name": "alice", "password": "alice-pass",
	}); code != 200 {
		t.Errorf("login after verifying = %d, want 200", code)
	}

	// The admin list shows the address and its state.
	admin := browser(t)
	if code, _ := call(t, admin, "POST", base+"/api/auth/login", map[string]string{
		"name": "admin", "password": "admin-pass",
	}); code != 200 {
		t.Fatal("admin login failed")
	}
	code, body = call(t, admin, "GET", base+"/api/admin/users", nil)
	if code != 200 {
		t.Fatalf("list users: %d", code)
	}
	if !userHasEmail(t, body, "alice", "alice@example.com", true) {
		t.Errorf("alice's verified address is missing from the list: %v", body)
	}
}

func userHasEmail(t *testing.T, body map[string]any, name, email string, verified bool) bool {
	t.Helper()
	list, _ := body["users"].([]any)
	for _, raw := range list {
		u, _ := raw.(map[string]any)
		if u["name"] != name {
			continue
		}
		return u["email"] == email && u["email_verified"] == verified
	}
	return false
}

// The safety property that matters most for an existing install: turning email
// verification on must not lock out the accounts that were already there, since
// they have no address and nothing to verify.
func TestTurningOnEmailVerificationDoesNotLockOutExistingAccounts(t *testing.T) {
	srv, ts := newTestServer(t)
	base := ts.URL
	admin := setupAdmin(t, base)

	// A user from before the feature existed: no address on file.
	if code, body := call(t, admin, "POST", base+"/api/admin/users", map[string]string{
		"name": "veteran", "password": "veteran-pass",
	}); code != 200 {
		t.Fatalf("could not create the pre-existing account: code %d body %v", code, body)
	}

	// Now the owner switches verification on.
	configureSMTP(t, srv)
	srv.Cfg.Users.RequireEmail = true

	for _, who := range []struct{ name, pass string }{
		{"admin", "admin-pass"},
		{"veteran", "veteran-pass"},
	} {
		if code, body := call(t, browser(t), "POST", base+"/api/auth/login", map[string]string{
			"name": who.name, "password": who.pass,
		}); code != 200 {
			t.Errorf("%s was locked out by the new setting: code %d body %v", who.name, code, body)
		}
	}

	// And an admin can attach an address by hand, vouching for it.
	code, body := call(t, admin, "GET", base+"/api/admin/users", nil)
	if code != 200 {
		t.Fatalf("list users: %d", code)
	}
	veteranID := idOf(t, body, "veteran")
	if code, body := call(t, admin, "PUT", base+"/api/admin/users/"+veteranID, map[string]any{
		"email": "veteran@example.com", "email_verified": true,
	}); code != 200 {
		t.Fatalf("admin set address: code %d body %v", code, body)
	}
	if code, _ := call(t, browser(t), "POST", base+"/api/auth/login", map[string]string{
		"name": "veteran", "password": "veteran-pass",
	}); code != 200 {
		t.Error("a hand-verified address still blocks sign-in")
	}
}

// Registration must refuse clearly when verification is on but no mail server is
// configured, rather than creating an account nobody can ever use.
func TestVerificationWithoutMailServerIsReported(t *testing.T) {
	srv, ts := newTestServer(t)
	base := ts.URL
	setupAdmin(t, base)
	srv.Cfg.Users.RequireEmail = true // no SMTP configured

	if code, body := call(t, browser(t), "GET", base+"/api/bootstrap", nil); code != 200 {
		t.Fatalf("bootstrap: %d", code)
	} else if body["can_register"] != false || body["mail_enabled"] != false {
		t.Errorf("bootstrap should say signup is not usable: %v", body)
	}
	code, body := call(t, browser(t), "POST", base+"/api/auth/register", map[string]string{
		"name": "someone", "password": "someone-pass", "email": "someone@example.com",
	})
	if code != 503 {
		t.Fatalf("register without a mail server = %d %v, want 503", code, body)
	}
	// No half-made account may be left behind.
	if _, err := srv.Store.UserByName("someone"); err == nil {
		t.Error("a registration that could not be verified still created an account")
	}
}

// Addresses are validated and cannot be shared between accounts.
func TestRegistrationEmailValidation(t *testing.T) {
	srv, ts := newTestServer(t)
	base := ts.URL
	setupAdmin(t, base)
	configureSMTP(t, srv)
	srv.Cfg.Users.RequireEmail = true

	if code, _ := call(t, browser(t), "POST", base+"/api/auth/register", map[string]string{
		"name": "noemail", "password": "whatever1",
	}); code != 400 {
		t.Error("signup without an address was accepted")
	}
	if code, _ := call(t, browser(t), "POST", base+"/api/auth/register", map[string]string{
		"name": "bademail", "password": "whatever1", "email": "not-an-address",
	}); code != 400 {
		t.Error("a malformed address was accepted")
	}
	if code, _ := call(t, browser(t), "POST", base+"/api/auth/register", map[string]string{
		"name": "first", "password": "whatever1", "email": "taken@example.com",
	}); code != 202 {
		t.Fatal("the first registration failed")
	}
	if code, _ := call(t, browser(t), "POST", base+"/api/auth/register", map[string]string{
		"name": "second", "password": "whatever1", "email": "TAKEN@example.com",
	}); code != 409 {
		t.Error("the same address was handed to a second account")
	}
}

// Forgot/reset: the link works once, ends every session, and is answered the
// same way for an account that does not exist.
func TestPasswordResetFlow(t *testing.T) {
	srv, ts := newTestServer(t)
	base := ts.URL
	admin := setupAdmin(t, base)
	fake := configureSMTP(t, srv)

	// Give the admin an address to reset to.
	code, body := call(t, admin, "PUT", base+"/api/admin/users/1", map[string]any{
		"email": "owner@example.com", "email_verified": true,
	})
	if code != 200 {
		t.Fatalf("set admin address: code %d body %v", code, body)
	}

	other := browser(t)
	if code, _ := call(t, other, "POST", base+"/api/auth/login", map[string]string{
		"name": "admin", "password": "admin-pass",
	}); code != 200 {
		t.Fatal("second device could not sign in")
	}

	// An unknown account is answered exactly like a known one, and no mail goes
	// out for it.
	code, unknown := call(t, browser(t), "POST", base+"/api/auth/forgot-password", map[string]string{
		"login": "nobody@example.com",
	})
	if code != 200 {
		t.Fatalf("forgot for an unknown account = %d", code)
	}
	if fake.Sent() != 0 {
		t.Error("a reset link was mailed for an account that does not exist")
	}

	code, known := call(t, browser(t), "POST", base+"/api/auth/forgot-password", map[string]string{
		"login": "owner@example.com",
	})
	if code != 200 {
		t.Fatalf("forgot: code %d body %v", code, known)
	}
	if unknown["message"] != known["message"] {
		t.Error("the answer differs between an existing and a missing account, which leaks who has one")
	}
	if fake.Sent() != 1 {
		t.Fatalf("reset messages = %d, want 1", fake.Sent())
	}
	token := tokenFromLink(t, fake.LinkWithPrefix(t, base+"/reset-password?token="), base+"/reset-password?token=")

	if code, _ := call(t, browser(t), "POST", base+"/api/auth/reset-password", map[string]string{
		"token": token, "password": "abc", // below the minimum
	}); code != 400 {
		t.Error("a too-short password was accepted by the reset form")
	}
	if code, body := call(t, browser(t), "POST", base+"/api/auth/reset-password", map[string]string{
		"token": token, "password": "new-admin-pass",
	}); code != 200 {
		t.Fatalf("reset: code %d body %v", code, body)
	}
	if code, _ := call(t, browser(t), "POST", base+"/api/auth/reset-password", map[string]string{
		"token": token, "password": "another-pass",
	}); code != 400 {
		t.Error("a reset token was accepted twice")
	}
	// Every session opened with the old password is gone.
	if code, _ := call(t, other, "GET", base+"/api/auth/me", nil); code != 401 {
		t.Errorf("an old session survived the reset (%d)", code)
	}
	if code, _ := call(t, browser(t), "POST", base+"/api/auth/login", map[string]string{
		"name": "admin", "password": "admin-pass",
	}); code != 401 {
		t.Error("the old password still works")
	}
	if code, _ := call(t, browser(t), "POST", base+"/api/auth/login", map[string]string{
		"name": "admin", "password": "new-admin-pass",
	}); code != 200 {
		t.Error("the new password does not work")
	}
}

func TestPasswordResetLinkInvalidAfterEmailChange(t *testing.T) {
	srv, ts := newTestServer(t)
	base := ts.URL
	admin := setupAdmin(t, base)
	fake := configureSMTP(t, srv)

	code, body := call(t, admin, "PUT", base+"/api/admin/users/1", map[string]any{
		"email": "before@example.com", "email_verified": true,
	})
	if code != 200 {
		t.Fatalf("set original email: code %d body %v", code, body)
	}
	if code, body := call(t, browser(t), "POST", base+"/api/auth/forgot-password",
		map[string]string{"login": "before@example.com"}); code != 200 {
		t.Fatalf("request reset: code %d body %v", code, body)
	}
	token := tokenFromLink(t, fake.LinkWithPrefix(t, base+"/reset-password?token="),
		base+"/reset-password?token=")

	if code, body := call(t, admin, "PUT", base+"/api/admin/users/1", map[string]any{
		"email": "after@example.com", "email_verified": false,
	}); code != 200 {
		t.Fatalf("change email: code %d body %v", code, body)
	}
	if code, body := call(t, browser(t), "POST", base+"/api/auth/reset-password",
		map[string]string{"token": token, "password": "attacker-pass"}); code != 400 {
		t.Fatalf("old reset link after email change = %d %v, want 400", code, body)
	}
	if code, _ := call(t, browser(t), "POST", base+"/api/auth/login", map[string]string{
		"name": "admin", "password": "admin-pass",
	}); code != 200 {
		t.Error("a reset link mailed to the previous address changed the password")
	}
}

// The reverse of the lockout test: with verification off, an unverified address
// is not a reason to refuse a sign-in.
func TestUnverifiedEmailDoesNotBlockWhenSettingIsOff(t *testing.T) {
	srv, ts := newTestServer(t)
	base := ts.URL
	setupAdmin(t, base)
	configureSMTP(t, srv)
	srv.Cfg.Users.RequireEmail = true

	alice := browser(t)
	if code, _ := call(t, alice, "POST", base+"/api/auth/register", map[string]string{
		"name": "alice", "password": "alice-pass", "email": "alice@example.com",
	}); code != 202 {
		t.Fatal("registration failed")
	}
	if code, _ := call(t, browser(t), "POST", base+"/api/auth/login", map[string]string{
		"name": "alice", "password": "alice-pass",
	}); code != 403 {
		t.Fatal("the unverified account was not held back")
	}
	// The owner gives up on mail and turns the requirement off.
	srv.Cfg.Users.RequireEmail = false
	if code, body := call(t, browser(t), "POST", base+"/api/auth/login", map[string]string{
		"name": "alice", "password": "alice-pass",
	}); code != 200 {
		t.Errorf("turning the setting off did not let the account in: code %d body %v", code, body)
	}
}

// Turnstile: enforced before the password, refused when Cloudflare says no, and
// refused — not waved through — when Cloudflare cannot be reached at all.
func TestTurnstileIsEnforced(t *testing.T) {
	srv, ts := newTestServer(t)
	base := ts.URL
	setupAdmin(t, base)

	// A local stand-in for challenges.cloudflare.com, so the test controls the
	// verdict.
	var allow bool
	var sawSecret, sawToken, sawIP string
	cf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sawSecret = r.PostFormValue("secret")
		sawToken = r.PostFormValue("response")
		sawIP = r.PostFormValue("remoteip")
		w.Header().Set("Content-Type", "application/json")
		if allow {
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response"]}`))
	}))
	defer cf.Close()
	restore := turnstileSiteverify
	turnstileSiteverify = cf.URL
	defer func() { turnstileSiteverify = restore }()

	srv.Cfg.Turnstile = config.Turnstile{Enabled: true, SiteKey: "site-key", SecretKey: "secret-key"}

	// With the keys in place the browser is told to render a widget.
	if code, body := call(t, browser(t), "GET", base+"/api/bootstrap", nil); code != 200 {
		t.Fatalf("bootstrap: %d", code)
	} else {
		ts, _ := body["turnstile"].(map[string]any)
		if ts == nil || ts["enabled"] != true || ts["site_key"] != "site-key" {
			t.Errorf("bootstrap did not advertise the widget: %v", body)
		}
	}

	// No token at all is refused without even asking Cloudflare.
	if code, body := call(t, browser(t), "POST", base+"/api/auth/login", map[string]string{
		"name": "admin", "password": "admin-pass",
	}); code != 400 || !strings.Contains(body["error"].(string), "人机验证") {
		t.Errorf("login without a token = %d %v", code, body)
	}

	// Cloudflare says the token is bad.
	allow = false
	if code, body := call(t, browser(t), "POST", base+"/api/auth/login", map[string]string{
		"name": "admin", "password": "admin-pass", "turnstile_token": "bad-token",
	}); code != 400 || !strings.Contains(body["error"].(string), "人机验证") {
		t.Errorf("login with a rejected token = %d %v", code, body)
	}
	if sawToken != "bad-token" || sawSecret != "secret-key" {
		t.Errorf("siteverify saw secret=%q token=%q", sawSecret, sawToken)
	}
	if sawIP == "" {
		t.Error("the visitor's address was not passed to siteverify")
	}

	// A good token lets the right password through.
	allow = true
	if code, body := call(t, browser(t), "POST", base+"/api/auth/login", map[string]string{
		"name": "admin", "password": "admin-pass", "turnstile_token": "good-token",
	}); code != 200 {
		t.Errorf("login with a good token = %d %v", code, body)
	}
	// And a good token does not rescue a wrong password.
	if code, _ := call(t, browser(t), "POST", base+"/api/auth/login", map[string]string{
		"name": "admin", "password": "wrong", "turnstile_token": "good-token",
	}); code != 401 {
		t.Errorf("a valid challenge let a wrong password through (%d)", code)
	}

	// Registration is protected too.
	if code, _ := call(t, browser(t), "POST", base+"/api/auth/register", map[string]string{
		"name": "newbie", "password": "newbie-pass",
	}); code != 400 {
		t.Error("registration was allowed without a challenge")
	}

	// Cloudflare unreachable: refuse rather than wave everyone through.
	turnstileSiteverify = "http://127.0.0.1:1/siteverify"
	if code, body := call(t, browser(t), "POST", base+"/api/auth/login", map[string]string{
		"name": "admin", "password": "admin-pass", "turnstile_token": "good-token",
	}); code != 400 || !strings.Contains(body["error"].(string), "Cloudflare") {
		t.Errorf("an unreachable siteverify let a login through: %d %v", code, body)
	}
}

// Half a Turnstile configuration must count as off, so a typo in one key cannot
// lock everyone out of the sign-in page.
func TestIncompleteTurnstileConfigurationIsIgnored(t *testing.T) {
	srv, ts := newTestServer(t)
	base := ts.URL
	setupAdmin(t, base)
	srv.Cfg.Turnstile = config.Turnstile{Enabled: true, SiteKey: "site-key"} // no secret

	if srv.Cfg.TurnstileEnabled() {
		t.Error("a half-configured Turnstile reports itself as enforcing")
	}
	if code, body := call(t, browser(t), "GET", base+"/api/bootstrap", nil); code != 200 {
		t.Fatalf("bootstrap: %d", code)
	} else if ts, _ := body["turnstile"].(map[string]any); ts["enabled"] != false {
		t.Errorf("bootstrap advertises an unusable widget: %v", body)
	}
	if code, _ := call(t, browser(t), "POST", base+"/api/auth/login", map[string]string{
		"name": "admin", "password": "admin-pass",
	}); code != 200 {
		t.Error("sign-in was blocked by a configuration that cannot challenge anybody")
	}
}

func TestEmailVerificationLinkInvalidAfterEmailChange(t *testing.T) {
	srv, ts := newTestServer(t)
	base := ts.URL
	admin := setupAdmin(t, base)
	fake := configureSMTP(t, srv)

	code, body := call(t, admin, "PUT", base+"/api/admin/users/1", map[string]any{
		"email": "before@example.com", "email_verified": false,
	})
	if code != 200 {
		t.Fatalf("set original email: code %d body %v", code, body)
	}
	if code, body := call(t, browser(t), "POST", base+"/api/auth/resend-verification",
		map[string]string{"login": "before@example.com"}); code != 200 {
		t.Fatalf("request verification: code %d body %v", code, body)
	}
	token := tokenFromLink(t, fake.LinkWithPrefix(t, base+"/verify-email?token="),
		base+"/verify-email?token=")

	if code, body := call(t, admin, "PUT", base+"/api/admin/users/1", map[string]any{
		"email": "after@example.com", "email_verified": false,
	}); code != 200 {
		t.Fatalf("change email: code %d body %v", code, body)
	}
	if code, body := call(t, browser(t), "POST", base+"/api/auth/verify-email",
		map[string]string{"token": token}); code != 400 {
		t.Fatalf("old verification link after email change = %d %v, want 400", code, body)
	}
	user, err := srv.Store.UserByID(1)
	if err != nil {
		t.Fatalf("read admin after rejected verification: %v", err)
	}
	if user.Email != "after@example.com" || user.EmailVerified {
		t.Fatalf("verification state after email change = %q verified=%v",
			user.Email, user.EmailVerified)
	}
}

// A signed-in user can attach an address to their own account, which is what
// makes password recovery possible for someone who registered before it existed.
func TestUserCanAttachTheirOwnEmail(t *testing.T) {
	srv, ts := newTestServer(t)
	base := ts.URL
	admin := setupAdmin(t, base)
	fake := configureSMTP(t, srv)

	code, body := call(t, admin, "POST", base+"/api/admin/users", map[string]string{
		"name": "viewer", "password": "viewer-pass",
	})
	if code != 200 {
		t.Fatalf("create viewer: %d %v", code, body)
	}
	viewer := browser(t)
	if code, _ := call(t, viewer, "POST", base+"/api/auth/login", map[string]string{
		"name": "viewer", "password": "viewer-pass",
	}); code != 200 {
		t.Fatal("viewer could not sign in")
	}
	if code, _ := call(t, viewer, "PUT", base+"/api/auth/email", map[string]string{
		"email": "not-an-address",
	}); code != 400 {
		t.Error("a malformed address was accepted")
	}
	code, body = call(t, viewer, "PUT", base+"/api/auth/email", map[string]string{
		"email": "viewer@example.com",
	})
	if code != 200 || body["ok"] != true {
		t.Fatalf("attach address: code %d body %v", code, body)
	}
	if fake.Sent() != 1 {
		t.Fatalf("verification messages = %d, want 1", fake.Sent())
	}
	// The account is not gated until the address is proven and the admin asks
	// for verification.
	if code, _ := call(t, viewer, "GET", base+"/api/auth/me", nil); code != 200 {
		t.Error("attaching an address signed the user out")
	}
	token := tokenFromLink(t, fake.LinkWithPrefix(t, base+"/verify-email?token="), base+"/verify-email?token=")
	if code, _ := call(t, browser(t), "POST", base+"/api/auth/verify-email", map[string]string{"token": token}); code != 200 {
		t.Fatal("the link from the user's own address did not verify")
	}
	// And now the address can be used to recover the account.
	if code, _ := call(t, browser(t), "POST", base+"/api/auth/forgot-password", map[string]string{
		"login": "viewer@example.com",
	}); code != 200 {
		t.Fatal("password recovery by address failed")
	}
	if fake.Sent() != 2 {
		t.Errorf("reset message missing: sent = %d", fake.Sent())
	}
}

// The admin's SMTP test button reports the real error instead of pretending.
func TestMailTestEndpoint(t *testing.T) {
	srv, ts := newTestServer(t)
	base := ts.URL
	admin := setupAdmin(t, base)
	fake := configureSMTP(t, srv)

	if code, body := call(t, admin, "POST", base+"/api/admin/mail/test", map[string]string{
		"to": "where@example.com",
	}); code != 200 || body["ok"] != true {
		t.Fatalf("mail test: code %d body %v", code, body)
	}
	if fake.Sent() != 1 {
		t.Fatalf("test messages = %d, want 1", fake.Sent())
	}

	// A broken configuration is reported as such, with HTTP 200 so the caller
	// gets the reason rather than a generic failure.
	srv.Cfg.SMTP.Port = 1
	code, body := call(t, admin, "POST", base+"/api/admin/mail/test", map[string]string{
		"to": "where@example.com",
	})
	if code != 200 || body["ok"] != false {
		t.Fatalf("broken mail test = %d %v, want ok:false", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "连接") {
		t.Errorf("error does not explain the failure: %v", body)
	}
}

// The mail limiter keeps an open "forgot password" form from being turned into a
// way to post mail at somebody else's inbox.
func TestPasswordResetMailIsRateLimited(t *testing.T) {
	srv, ts := newTestServer(t)
	base := ts.URL
	admin := setupAdmin(t, base)
	fake := configureSMTP(t, srv)
	if code, _ := call(t, admin, "PUT", base+"/api/admin/users/1", map[string]any{
		"email": "owner@example.com", "email_verified": true,
	}); code != 200 {
		t.Fatal("could not attach an address")
	}

	c := browser(t)
	throttled := false
	for i := 0; i < 10; i++ {
		code, _ := call(t, c, "POST", base+"/api/auth/forgot-password", map[string]string{
			"login": "owner@example.com",
		})
		if code == 429 {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Error("ten reset requests in a row were never throttled")
	}
	if fake.Sent() > 5 {
		t.Errorf("the limiter let %d messages out, want at most 5", fake.Sent())
	}
}

// The config round-trip carries the mail and CAPTCHA settings, and refuses a
// value it cannot honour.
func TestMailAndTurnstileConfigRoundTrip(t *testing.T) {
	_, ts := newTestServer(t)
	base := ts.URL
	admin := setupAdmin(t, base)

	// Who may read these settings is checked first, while sign-in still works
	// without a CAPTCHA: a signed-out caller gets 401, a viewer 403.
	if code, _ := call(t, browser(t), "GET", base+"/api/config", nil); code != 401 {
		t.Error("a signed-out caller read the SMTP settings")
	}
	if code, _ := call(t, admin, "POST", base+"/api/admin/users", map[string]string{
		"name": "nosey", "password": "nosey-pass",
	}); code != 200 {
		t.Fatal("could not create the viewer")
	}
	nosey := browser(t)
	if code, body := call(t, nosey, "POST", base+"/api/auth/login", map[string]string{
		"name": "nosey", "password": "nosey-pass",
	}); code != 200 {
		t.Fatalf("viewer could not sign in: code %d body %v", code, body)
	}
	if code, _ := call(t, nosey, "GET", base+"/api/config", nil); code != 403 {
		t.Error("a viewer read the SMTP settings")
	}

	code, body := call(t, admin, "PUT", base+"/api/config", map[string]any{
		"smtp": map[string]any{
			"host": "smtp.example.com", "port": 465, "username": "robot@example.com",
			"password": "app-password", "from_name": "DoriyaMedia 影音", "encryption": "tls",
		},
		"turnstile": map[string]any{"enabled": true, "site_key": "sk", "secret_key": "sec"},
		"users":     map[string]any{"require_email": true, "base_url": "https://media.example.com/"},
	})
	if code != 200 {
		t.Fatalf("save config: code %d body %v", code, body)
	}
	code, body = call(t, admin, "GET", base+"/api/config", nil)
	if code != 200 {
		t.Fatalf("read config: %d", code)
	}
	smtp, _ := body["smtp"].(map[string]any)
	if smtp["host"] != "smtp.example.com" || smtp["encryption"] != "tls" {
		t.Errorf("smtp did not round-trip: %v", smtp)
	}
	users, _ := body["users"].(map[string]any)
	// The trailing slash is stripped so links do not come out as "//verify-email".
	if users["base_url"] != "https://media.example.com" || users["require_email"] != true {
		t.Errorf("users did not round-trip: %v", users)
	}
	turnstile, _ := body["turnstile"].(map[string]any)
	if turnstile["enabled"] != true || turnstile["site_key"] != "sk" {
		t.Errorf("turnstile did not round-trip: %v", turnstile)
	}
	if code, _ := call(t, admin, "PUT", base+"/api/config", map[string]any{
		"smtp": map[string]any{"encryption": "rot13"},
	}); code != 400 {
		t.Error("an impossible encryption mode was accepted")
	}
}

// jsonNumber is used by idOf; this keeps the helper honest for the new tests.
func TestJSONNumberHelper(t *testing.T) {
	var v any
	if err := json.Unmarshal([]byte(`{"id":42}`), &v); err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if got := jsonNumber(m["id"]); got != "42" {
		t.Errorf("jsonNumber = %q, want 42", got)
	}
}
