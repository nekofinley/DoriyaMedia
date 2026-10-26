package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"doriyamedia/internal/auth"
	"doriyamedia/internal/db"
	"doriyamedia/internal/models"
)

const (
	v1DeviceAuthPollInterval = 5
	v1JSONBodyLimit          = 1 << 20
	v1DeviceNameMaxRunes     = 80
	v1ClientVersionMaxBytes  = 64
)

var (
	v1AuthorizationIDPattern = regexp.MustCompile(`^dva_[A-Za-z0-9_-]{22,96}$`)
	v1DeviceIDPattern        = regexp.MustCompile(`^dev_[A-Za-z0-9_-]{8,96}$`)
	v1ClientVersionPattern   = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]*$`)
	v1CredentialPattern      = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
)

type v1DeviceAuthorizationStartResponse struct {
	AuthorizationID         string `json:"authorization_id"`
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type v1DeviceAuthorizationStatusResponse struct {
	State     string    `json:"state"`
	ExpiresAt time.Time `json:"expires_at"`
}

type v1DeviceAuthorizationLookupResponse struct {
	AuthorizationID string    `json:"authorization_id"`
	DeviceName      string    `json:"device_name"`
	Platform        string    `json:"platform"`
	ClientVersion   string    `json:"client_version"`
	State           string    `json:"state"`
	ExpiresAt       time.Time `json:"expires_at"`
	CSRFToken       string    `json:"csrf_token,omitempty"`
}

type v1DeviceAuthorizationDecisionResponse struct {
	State string `json:"state"`
}

type v1DeviceTokenPendingResponse struct {
	State             string    `json:"state"`
	RetryAfterSeconds int       `json:"retry_after_seconds"`
	ExpiresAt         time.Time `json:"expires_at"`
}

type v1DeviceTokenPairResponse struct {
	AccessToken      string         `json:"access_token"`
	TokenType        string         `json:"token_type"`
	ExpiresIn        int            `json:"expires_in"`
	RefreshToken     string         `json:"refresh_token"`
	RefreshExpiresIn int            `json:"refresh_expires_in"`
	Device           v1NativeDevice `json:"device"`
	User             v1AuthUser     `json:"user"`
}

type v1NativeDevice struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Platform      string     `json:"platform"`
	ClientVersion string     `json:"client_version"`
	CreatedAt     time.Time  `json:"created_at"`
	LastSeenAt    time.Time  `json:"last_seen_at"`
	RevokedAt     *time.Time `json:"revoked_at"`
}

type v1AuthUser struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type v1DeviceListResponse struct {
	Devices []v1NativeDevice `json:"devices"`
}

type v1DeviceRevocationResponse struct {
	DeviceID  string    `json:"device_id"`
	State     string    `json:"state"`
	RevokedAt time.Time `json:"revoked_at"`
}

type v1DeviceAccessContext struct {
	User      *models.User
	Device    *models.Device
	Overrides models.DeviceFeatureOverrides
}

type v1DeviceAccessHandler func(http.ResponseWriter, *http.Request, *v1DeviceAccessContext)

// requireV1DeviceAuthFeature is the fail-closed gate shared by every S2 route.
// Discovery (/api/v1/meta) deliberately bypasses it.
func (s *Server) requireV1DeviceAuthFeature(w http.ResponseWriter, r *http.Request) bool {
	if s == nil || s.Cfg == nil || !s.Cfg.Client.V1Enabled || !s.Cfg.Client.DeviceAuthEnabled {
		writeV1Error(w, http.StatusNotFound, V1ErrorFeatureDisabled, "The device authentication feature is disabled.", nil)
		return false
	}
	return true
}

func (s *Server) requireV1DeviceAccess(next v1DeviceAccessHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.requireV1DeviceAuthFeature(w, r) {
			return
		}
		token, ok := v1BearerToken(r)
		if !ok {
			writeV1Error(w, http.StatusUnauthorized, V1ErrorUnauthenticated, "A device access token is required.", nil)
			return
		}
		if s.Store == nil {
			writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not complete the request.", nil)
			return
		}
		requireEmailVerification := s.Cfg != nil && s.Cfg.EmailVerificationRequired()
		ctx, err := s.Store.DeviceByAccessTokenWithEmailPolicy(token, requireEmailVerification, time.Now())
		if err != nil {
			writeV1DeviceAccessError(w, err)
			return
		}
		if !s.userAllowedForNativeClient(ctx.User) {
			writeV1Error(w, http.StatusForbidden, V1ErrorForbidden, "The account cannot use device authentication.", nil)
			return
		}
		features := s.effectiveClientFeatures(ctx.Overrides)
		if !features.V1Enabled || !features.DeviceAuthEnabled {
			writeV1DeviceFeatureDisabled(w)
			return
		}
		next(w, r, &v1DeviceAccessContext{
			User: ctx.User, Device: ctx.Device, Overrides: ctx.Overrides,
		})
	}
}

func v1BearerToken(r *http.Request) (string, bool) {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func writeV1DeviceAccessError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrInvalidDeviceAccessToken),
		errors.Is(err, db.ErrDeviceAccessTokenExpired),
		errors.Is(err, db.ErrDeviceAccessTokenRevoked):
		writeV1Error(w, http.StatusUnauthorized, V1ErrorUnauthenticated, "The device access token is invalid or expired.", nil)
	case errors.Is(err, db.ErrDeviceRevoked):
		writeV1Error(w, http.StatusUnauthorized, V1ErrorDeviceRevoked, "This device has been revoked.", map[string]any{
			"action": "reauth",
		})
	case errors.Is(err, db.ErrDeviceV1Disabled),
		errors.Is(err, db.ErrDeviceAuthFeatureDisabled):
		writeV1DeviceFeatureDisabled(w)
	case errors.Is(err, db.ErrDeviceUserUnavailable):
		writeV1Error(w, http.StatusForbidden, V1ErrorForbidden, "The account cannot use device authentication.", nil)
	default:
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not verify the device.", nil)
	}
}

func decodeV1JSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if err := requireV1JSONContentType(r); err != nil {
		return err
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, v1JSONBodyLimit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func requireV1JSONContentType(r *http.Request) error {
	contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if contentType == "" {
		return errors.New("Content-Type must be application/json")
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return errors.New("Content-Type must be application/json")
	}
	return nil
}

func (s *Server) allowV1DeviceRequest(w http.ResponseWriter, limiter *attemptLimiter, key, scope string) bool {
	if limiter == nil || limiter.Allow(key) {
		if limiter != nil {
			_ = limiter.Hit(key)
		}
		return true
	}
	retryAfter := 1
	if limiter.window > 0 {
		retryAfter = int(limiter.window/time.Second) + 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	writeV1Error(w, http.StatusTooManyRequests, V1ErrorRateLimited, "Too many requests. Try again later.", map[string]any{
		"retry_after_seconds": retryAfter,
		"scope":               scope,
	})
	return false
}

func (s *Server) v1DeviceUserIPKey(user *models.User, r *http.Request) string {
	userID := int64(0)
	if user != nil {
		userID = user.ID
	}
	return fmt.Sprintf("%d|%s", userID, clientIP(r))
}

func normalizeV1DeviceName(raw string) (string, error) {
	var cleaned strings.Builder
	for _, r := range raw {
		if unicode.IsControl(r) {
			continue
		}
		cleaned.WriteRune(r)
	}
	name := strings.TrimSpace(cleaned.String())
	count := utf8.RuneCountInString(name)
	if count < 1 || count > v1DeviceNameMaxRunes {
		return "", fmt.Errorf("device_name must be 1-%d characters", v1DeviceNameMaxRunes)
	}
	return name, nil
}

func normalizeV1ClientVersion(raw string) (string, error) {
	version := strings.TrimSpace(raw)
	if len(version) < 1 || len(version) > v1ClientVersionMaxBytes || !v1ClientVersionPattern.MatchString(version) {
		return "", fmt.Errorf("client_version must be 1-%d ASCII characters", v1ClientVersionMaxBytes)
	}
	return version, nil
}

func normalizeV1Platform(raw string) (string, error) {
	switch platform := strings.ToLower(strings.TrimSpace(raw)); platform {
	case "windows", "android":
		return platform, nil
	default:
		return "", errors.New("platform must be windows or android")
	}
}

func (s *Server) v1DeviceVerificationBaseURL(r *http.Request) (string, error) {
	configured := strings.TrimSpace(s.Cfg.Users.BaseURL)
	raw := configured
	if raw == "" {
		scheme := "http"
		if r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
			scheme = "https"
		}
		host := strings.TrimSpace(r.Host)
		if host == "" {
			host = "127.0.0.1:" + strconv.Itoa(s.Cfg.Port)
		}
		raw = scheme + "://" + host
	}

	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid verification base URL")
	}
	// A Host header must not smuggle a path into the fixed /device entry point.
	if configured == "" && u.EscapedPath() != "" && u.EscapedPath() != "/" {
		return "", errors.New("invalid verification host")
	}

	u.Path = strings.TrimRight(u.Path, "/") + "/device"
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func (s *Server) handleV1DeviceAuthorizationStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireV1DeviceAuthFeature(w, r) {
		return
	}
	if !s.allowV1DeviceRequest(w, s.deviceAuthStartLimiter, clientIP(r), "device_authorization_start") {
		return
	}
	var req struct {
		DeviceName          string `json:"device_name"`
		Platform            string `json:"platform"`
		ClientVersion       string `json:"client_version"`
		CodeChallenge       string `json:"code_challenge"`
		CodeChallengeMethod string `json:"code_challenge_method"`
	}
	if err := decodeV1JSON(w, r, &req); err != nil {
		writeV1Error(w, http.StatusBadRequest, V1ErrorInvalidRequest, "The device authorization request is invalid.", nil)
		return
	}
	deviceName, err := normalizeV1DeviceName(req.DeviceName)
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, V1ErrorInvalidRequest, err.Error(), nil)
		return
	}
	platform, err := normalizeV1Platform(req.Platform)
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, V1ErrorInvalidRequest, err.Error(), nil)
		return
	}
	clientVersion, err := normalizeV1ClientVersion(req.ClientVersion)
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, V1ErrorInvalidRequest, err.Error(), nil)
		return
	}
	if req.CodeChallengeMethod != auth.PKCEMethodS256 || !auth.ValidatePKCEChallenge(req.CodeChallenge) {
		writeV1Error(w, http.StatusBadRequest, V1ErrorInvalidRequest, "code_challenge must be a valid PKCE S256 challenge.", nil)
		return
	}
	baseURL, err := s.v1DeviceVerificationBaseURL(r)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not build the verification URL.", nil)
		return
	}
	if s.Store == nil {
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not create the authorization.", nil)
		return
	}
	grant, err := s.Store.CreateDeviceAuthorization(models.DeviceAuthorizationRequest{
		DeviceName:          deviceName,
		Platform:            platform,
		ClientVersion:       clientVersion,
		CodeChallenge:       req.CodeChallenge,
		CodeChallengeMethod: req.CodeChallengeMethod,
	}, time.Now())
	if err != nil {
		switch {
		case errors.Is(err, db.ErrInvalidDeviceMetadata),
			errors.Is(err, db.ErrInvalidDevicePKCEChallenge):
			writeV1Error(w, http.StatusBadRequest, V1ErrorInvalidRequest, "The device authorization request is invalid.", nil)
		default:
			writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not create the authorization.", nil)
		}
		return
	}

	setV1NoStore(w)
	writeJSON(w, http.StatusCreated, v1DeviceAuthorizationStartResponse{
		AuthorizationID:         grant.Authorization.AuthorizationID,
		DeviceCode:              grant.DeviceCode,
		UserCode:                grant.UserCode,
		VerificationURI:         baseURL,
		VerificationURIComplete: baseURL + "?code=" + url.QueryEscape(grant.UserCode),
		ExpiresIn:               int(auth.DeviceAuthorizationTTL / time.Second),
		Interval:                v1DeviceAuthPollInterval,
	})
}

func (s *Server) handleV1DeviceAuthorizationStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireV1DeviceAuthFeature(w, r) {
		return
	}
	authorizationID := strings.TrimSpace(r.PathValue("id"))
	if !v1AuthorizationIDPattern.MatchString(authorizationID) {
		writeV1AuthorizationNotFound(w)
		return
	}
	if s.Store == nil {
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not read the authorization.", nil)
		return
	}
	authorization, err := s.Store.DeviceAuthorizationByID(authorizationID, time.Now())
	if err != nil {
		if errors.Is(err, db.ErrDeviceAuthorizationNotFound) {
			writeV1AuthorizationNotFound(w)
			return
		}
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not read the authorization.", nil)
		return
	}
	writeJSON(w, http.StatusOK, v1DeviceAuthorizationStatusResponse{
		State:     authorization.State,
		ExpiresAt: authorization.ExpiresAt,
	})
}

func (s *Server) handleV1DeviceAuthorizationLookup(w http.ResponseWriter, r *http.Request) {
	if !s.requireV1DeviceAuthFeature(w, r) {
		return
	}
	if s.Store == nil {
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not look up the authorization.", nil)
		return
	}
	user := s.currentUser(r)
	if user == nil {
		writeV1Error(w, http.StatusUnauthorized, V1ErrorUnauthenticated, "A Web session is required.", nil)
		return
	}
	if !s.allowV1DeviceRequest(w, s.deviceAuthLookupLimiter, s.v1DeviceUserIPKey(user, r), "device_authorization_lookup") {
		return
	}
	userCode := strings.TrimSpace(r.URL.Query().Get("user_code"))
	if userCode == "" {
		writeV1Error(w, http.StatusBadRequest, V1ErrorInvalidRequest, "user_code is required.", nil)
		return
	}
	if s.Store == nil {
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not look up the authorization.", nil)
		return
	}
	authorization, err := s.Store.DeviceAuthorizationByUserCode(userCode, time.Now())
	if err != nil {
		switch {
		case errors.Is(err, db.ErrInvalidDeviceUserCode):
			writeV1Error(w, http.StatusBadRequest, V1ErrorInvalidRequest, "The user code is invalid.", nil)
		case errors.Is(err, db.ErrDeviceAuthorizationNotFound):
			writeV1AuthorizationNotFound(w)
		case errors.Is(err, db.ErrDeviceAuthorizationDenied):
			writeV1Error(w, http.StatusForbidden, V1ErrorAuthorizationDenied, "The device authorization was denied.", map[string]any{
				"action": "start_new_authorization",
			})
		case errors.Is(err, db.ErrDeviceAuthorizationExpired):
			writeV1Error(w, http.StatusGone, V1ErrorAuthorizationExpired, "The device authorization has expired.", map[string]any{
				"action": "start_new_authorization",
			})
		case errors.Is(err, db.ErrDeviceAuthorizationConsumed):
			writeV1Error(w, http.StatusConflict, V1ErrorAuthorizationConsumed, "The device authorization has already been used.", map[string]any{
				"action": "start_new_authorization",
			})
		default:
			writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not look up the authorization.", nil)
		}
		return
	}
	response := v1DeviceAuthorizationLookupResponse{
		AuthorizationID: authorization.AuthorizationID,
		DeviceName:      authorization.DeviceName,
		Platform:        authorization.Platform,
		ClientVersion:   authorization.ClientVersion,
		State:           authorization.State,
		ExpiresAt:       authorization.ExpiresAt,
	}
	if authorization.State == models.DeviceAuthorizationPending {
		csrf, err := s.Store.IssueDeviceAuthorizationCSRF(authorization.AuthorizationID, user.ID, time.Now())
		if err != nil {
			switch {
			case errors.Is(err, db.ErrDeviceAuthorizationExpired):
				writeV1Error(w, http.StatusGone, V1ErrorAuthorizationExpired, "The device authorization has expired.", map[string]any{
					"action": "start_new_authorization",
				})
			case errors.Is(err, db.ErrDeviceAuthorizationAlreadyDecided):
				writeV1Error(w, http.StatusConflict, V1ErrorAuthorizationConsumed, "The device authorization has already been decided.", nil)
			case errors.Is(err, db.ErrDeviceAuthorizationNotFound):
				writeV1AuthorizationNotFound(w)
			default:
				writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not prepare the approval.", nil)
			}
			return
		}
		response.CSRFToken = csrf.Token
	}
	setV1NoStore(w)
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleV1DeviceAuthorizationDecision(w http.ResponseWriter, r *http.Request) {
	if !s.requireV1DeviceAuthFeature(w, r) {
		return
	}
	if s.Store == nil {
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not record the decision.", nil)
		return
	}
	user := s.currentUser(r)
	if user == nil {
		writeV1Error(w, http.StatusUnauthorized, V1ErrorUnauthenticated, "A Web session is required.", nil)
		return
	}
	if !s.v1SameOrigin(r) {
		writeV1Error(w, http.StatusForbidden, V1ErrorForbidden, "The request origin is not allowed.", nil)
		return
	}
	if !s.v1SameOriginFetch(r) {
		writeV1Error(w, http.StatusForbidden, V1ErrorForbidden, "The request site is not allowed.", nil)
		return
	}
	if !s.allowV1DeviceRequest(w, s.deviceAuthDecisionLimiter, s.v1DeviceUserIPKey(user, r), "device_authorization_decision") {
		return
	}
	authorizationID := strings.TrimSpace(r.PathValue("id"))
	if !v1AuthorizationIDPattern.MatchString(authorizationID) {
		writeV1AuthorizationNotFound(w)
		return
	}
	var req struct {
		Decision string `json:"decision"`
	}
	if err := decodeV1JSON(w, r, &req); err != nil {
		writeV1Error(w, http.StatusBadRequest, V1ErrorInvalidRequest, "The decision request must be JSON.", nil)
		return
	}
	if req.Decision != "approve" && req.Decision != "deny" {
		writeV1Error(w, http.StatusBadRequest, V1ErrorInvalidRequest, "decision must be approve or deny.", nil)
		return
	}
	csrfToken := strings.TrimSpace(r.Header.Get("X-CSRF-Token"))
	if !validV1Credential(csrfToken, 32) {
		writeV1CSRFInvalid(w)
		return
	}
	if s.Store == nil {
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not record the decision.", nil)
		return
	}

	now := time.Now()
	var (
		authorization *models.DeviceAuthorization
		err           error
	)
	if req.Decision == "approve" {
		authorization, err = s.Store.DecideDeviceAuthorizationByIDWithCSRF(
			authorizationID, user.ID, csrfToken, true, now)
	} else {
		authorization, err = s.Store.DecideDeviceAuthorizationByIDWithCSRF(
			authorizationID, user.ID, csrfToken, false, now)
	}
	if err != nil {
		switch {
		case errors.Is(err, db.ErrDeviceAuthorizationNotFound):
			writeV1AuthorizationNotFound(w)
		case errors.Is(err, db.ErrDeviceAuthorizationExpired):
			writeV1Error(w, http.StatusGone, V1ErrorAuthorizationExpired, "The device authorization has expired.", map[string]any{
				"action": "start_new_authorization",
			})
		case errors.Is(err, db.ErrDeviceAuthorizationAlreadyDecided):
			writeV1Error(w, http.StatusConflict, V1ErrorAuthorizationConsumed, "The device authorization has already been decided.", nil)
		case errors.Is(err, db.ErrDeviceUserUnavailable):
			writeV1Error(w, http.StatusForbidden, V1ErrorForbidden, "The account cannot approve device authorizations.", nil)
		case errors.Is(err, db.ErrDeviceAuthorizationCSRF):
			writeV1CSRFInvalid(w)
		default:
			writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not record the decision.", nil)
		}
		return
	}
	writeJSON(w, http.StatusOK, v1DeviceAuthorizationDecisionResponse{State: authorization.State})
}

func (s *Server) v1SameOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return strings.EqualFold(u.Scheme, s.v1RequestScheme(r)) && strings.EqualFold(u.Host, r.Host)
}

func (s *Server) v1SameOriginFetch(r *http.Request) bool {
	site := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))
	return site == "" || strings.EqualFold(site, "same-origin")
}

func (s *Server) v1RequestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if s.Cfg != nil {
		baseURL := strings.TrimSpace(s.Cfg.Users.BaseURL)
		if u, err := url.Parse(baseURL); err == nil && strings.EqualFold(u.Host, r.Host) &&
			(u.Scheme == "http" || u.Scheme == "https") {
			return u.Scheme
		}
	}
	proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	if strings.EqualFold(proto, "https") {
		return "https"
	}
	return "http"
}

func (s *Server) handleV1DeviceToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireV1DeviceAuthFeature(w, r) {
		return
	}
	if !s.allowV1DeviceRequest(w, s.deviceAuthTokenLimiter, clientIP(r), "device_token") {
		return
	}
	var req struct {
		DeviceCode   string `json:"device_code"`
		CodeVerifier string `json:"code_verifier"`
	}
	if err := decodeV1JSON(w, r, &req); err != nil {
		writeV1Error(w, http.StatusBadRequest, V1ErrorInvalidRequest, "The token request must be JSON.", nil)
		return
	}
	if !validV1Credential(req.DeviceCode, 32) || auth.ValidatePKCEVerifier(req.CodeVerifier) != nil {
		writeV1InvalidGrant(w)
		return
	}
	if s.Store == nil {
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not exchange the authorization.", nil)
		return
	}
	requireEmailVerification := s.Cfg != nil && s.Cfg.EmailVerificationRequired()
	pair, err := s.Store.ConsumeDeviceAuthorizationWithEmailPolicy(
		req.DeviceCode, req.CodeVerifier, requireEmailVerification, time.Now())
	if err != nil {
		switch {
		case errors.Is(err, db.ErrDeviceAuthorizationPending):
			expiresAt, expiryErr := s.v1DeviceAuthorizationExpiresAt(req.DeviceCode)
			if expiryErr != nil {
				writeV1InvalidGrant(w)
				return
			}
			setV1NoStore(w)
			writeJSON(w, http.StatusAccepted, v1DeviceTokenPendingResponse{
				State:             models.DeviceAuthorizationPending,
				RetryAfterSeconds: v1DeviceAuthPollInterval,
				ExpiresAt:         expiresAt,
			})
		case errors.Is(err, db.ErrDeviceAuthorizationDenied):
			writeV1Error(w, http.StatusForbidden, V1ErrorAuthorizationDenied, "The device authorization was denied.", map[string]any{
				"action": "start_new_authorization",
			})
		case errors.Is(err, db.ErrDeviceAuthorizationExpired):
			writeV1Error(w, http.StatusGone, V1ErrorAuthorizationExpired, "The device authorization has expired.", map[string]any{
				"action": "start_new_authorization",
			})
		case errors.Is(err, db.ErrDeviceAuthorizationConsumed):
			writeV1Error(w, http.StatusConflict, V1ErrorAuthorizationConsumed, "The device authorization has already been used.", map[string]any{
				"action": "start_new_authorization",
			})
		case errors.Is(err, db.ErrInvalidDeviceCode),
			errors.Is(err, db.ErrInvalidDevicePKCEVerifier):
			writeV1InvalidGrant(w)
		case errors.Is(err, db.ErrDeviceUserUnavailable):
			writeV1Error(w, http.StatusForbidden, V1ErrorForbidden, "The account cannot complete device authentication.", nil)
		default:
			writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not exchange the authorization.", nil)
		}
		return
	}
	if err := s.writeV1DeviceTokenPair(w, pair); err != nil {
		if errors.Is(err, db.ErrDeviceUserUnavailable) {
			writeV1Error(w, http.StatusForbidden, V1ErrorForbidden, "The account cannot complete device authentication.", nil)
			return
		}
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not finish issuing tokens.", nil)
	}
}

func validV1Credential(value string, minLength int) bool {
	return len(value) >= minLength && len(value) <= 512 && v1CredentialPattern.MatchString(value)
}

func writeV1InvalidGrant(w http.ResponseWriter) {
	writeV1Error(w, http.StatusBadRequest, V1ErrorInvalidGrant, "The device code, verifier, or authorization is invalid.", nil)
}

func writeV1CSRFInvalid(w http.ResponseWriter) {
	writeV1Error(w, http.StatusForbidden, V1ErrorForbidden, "The approval token is invalid or expired. Look up the authorization again.", map[string]any{
		"action": "reload_authorization",
	})
}

func (s *Server) v1DeviceAuthorizationExpiresAt(deviceCode string) (time.Time, error) {
	if s.Store == nil {
		return time.Time{}, errors.New("store is unavailable")
	}
	var expiresAt time.Time
	err := s.Store.DB.QueryRow(`SELECT expires_at FROM device_authorizations
		WHERE device_code_hash = ?`, auth.HashDeviceCredential(deviceCode)).Scan(&expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, db.ErrInvalidDeviceCode
	}
	return expiresAt, err
}

func (s *Server) handleV1DeviceRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.requireV1DeviceAuthFeature(w, r) {
		return
	}
	if !s.allowV1DeviceRequest(w, s.deviceAuthRefreshLimiter, clientIP(r), "device_refresh") {
		return
	}
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := decodeV1JSON(w, r, &req); err != nil {
		writeV1Error(w, http.StatusBadRequest, V1ErrorInvalidRequest, "The refresh request must be JSON.", nil)
		return
	}
	if !validV1Credential(req.RefreshToken, 32) {
		writeV1RefreshTokenInvalid(w)
		return
	}
	if s.Store == nil {
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not rotate the refresh token.", nil)
		return
	}
	requireEmailVerification := s.Cfg != nil && s.Cfg.EmailVerificationRequired()
	pair, err := s.Store.RotateDeviceRefreshTokenWithEmailPolicy(
		req.RefreshToken, requireEmailVerification, time.Now())
	if err != nil {
		switch {
		case errors.Is(err, db.ErrDeviceRefreshTokenReused):
			writeV1Error(w, http.StatusConflict, V1ErrorRefreshReuseDetected, "This refresh token was already used. Sign in on the device again.", map[string]any{
				"action":         "reauth",
				"family_revoked": true,
			})
		case errors.Is(err, db.ErrInvalidDeviceRefreshToken),
			errors.Is(err, db.ErrDeviceRefreshTokenExpired),
			errors.Is(err, db.ErrDeviceRefreshTokenRevoked),
			errors.Is(err, db.ErrDeviceV1Disabled),
			errors.Is(err, db.ErrDeviceAuthFeatureDisabled),
			errors.Is(err, db.ErrDeviceUserUnavailable),
			errors.Is(err, db.ErrDeviceRevoked):
			// All of these look identical so a caller cannot enumerate whether
			// a token belongs to a disabled account or a revoked device.
			writeV1RefreshTokenInvalid(w)
		default:
			writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not rotate the refresh token.", nil)
		}
		return
	}
	if err := s.writeV1DeviceTokenPair(w, pair); err != nil {
		if errors.Is(err, db.ErrDeviceUserUnavailable) {
			writeV1RefreshTokenInvalid(w)
			return
		}
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not finish rotating tokens.", nil)
	}
}

func writeV1RefreshTokenInvalid(w http.ResponseWriter) {
	writeV1Error(w, http.StatusUnauthorized, V1ErrorRefreshTokenInvalid, "The refresh token is invalid or expired.", map[string]any{
		"action": "reauth",
	})
}

func (s *Server) writeV1DeviceTokenPair(w http.ResponseWriter, pair *models.DeviceTokenPair) error {
	if pair == nil || pair.Device == nil {
		return errors.New("device token pair is incomplete")
	}
	user, err := s.Store.UserByID(pair.Device.UserID)
	if err != nil {
		return err
	}
	if !s.userAllowedForNativeClient(user) {
		return db.ErrDeviceUserUnavailable
	}
	refreshExpiresIn := pair.RefreshExpiresIn
	if refreshExpiresIn <= 0 {
		refreshExpiresIn = int(pair.RefreshExpiresAt.Sub(time.Now()) / time.Second)
		if refreshExpiresIn < 0 {
			refreshExpiresIn = 0
		}
	}
	setV1NoStore(w)
	writeJSON(w, http.StatusOK, v1DeviceTokenPairResponse{
		AccessToken:      pair.AccessToken,
		TokenType:        "Bearer",
		ExpiresIn:        int(auth.DeviceAccessTokenTTL / time.Second),
		RefreshToken:     pair.RefreshToken,
		RefreshExpiresIn: refreshExpiresIn,
		Device:           v1NativeDeviceFromModel(*pair.Device),
		User: v1AuthUser{
			ID:   user.ID,
			Name: user.Name,
		},
	})
	return nil
}

func (s *Server) handleV1DeviceLogout(w http.ResponseWriter, r *http.Request, ctx *v1DeviceAccessContext) {
	if s.Store == nil || ctx == nil || ctx.User == nil || ctx.Device == nil {
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not revoke the device.", nil)
		return
	}
	if err := s.Store.RevokeDevice(ctx.User.ID, ctx.Device.PublicID, time.Now()); err != nil {
		if errors.Is(err, db.ErrDeviceNotFound) {
			writeV1Error(w, http.StatusNotFound, V1ErrorDeviceNotFound, "The device was not found.", nil)
			return
		}
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not revoke the device.", nil)
		return
	}
	setV1NoStore(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleV1DeviceList(w http.ResponseWriter, r *http.Request, ctx *v1DeviceAccessContext) {
	if s.Store == nil || ctx == nil || ctx.User == nil {
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not list devices.", nil)
		return
	}
	devices, err := s.Store.DevicesByUser(ctx.User.ID)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not list devices.", nil)
		return
	}
	if len(devices) > 100 {
		devices = devices[:100]
	}
	response := v1DeviceListResponse{Devices: make([]v1NativeDevice, 0, len(devices))}
	for _, device := range devices {
		response.Devices = append(response.Devices, v1NativeDeviceFromModel(device))
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleV1DeviceDelete(w http.ResponseWriter, r *http.Request, ctx *v1DeviceAccessContext) {
	if s.Store == nil || ctx == nil || ctx.User == nil {
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not revoke the device.", nil)
		return
	}
	deviceID := strings.TrimSpace(r.PathValue("id"))
	if !v1DeviceIDPattern.MatchString(deviceID) {
		writeV1Error(w, http.StatusNotFound, V1ErrorDeviceNotFound, "The device was not found.", nil)
		return
	}
	if err := s.Store.RevokeDevice(ctx.User.ID, deviceID, time.Now()); err != nil {
		if errors.Is(err, db.ErrDeviceNotFound) {
			writeV1Error(w, http.StatusNotFound, V1ErrorDeviceNotFound, "The device was not found.", nil)
			return
		}
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not revoke the device.", nil)
		return
	}
	devices, err := s.Store.DevicesByUser(ctx.User.ID)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not read the revoked device.", nil)
		return
	}
	for _, device := range devices {
		if device.PublicID != deviceID || device.RevokedAt == nil {
			continue
		}
		writeJSON(w, http.StatusOK, v1DeviceRevocationResponse{
			DeviceID:  device.PublicID,
			State:     "revoked",
			RevokedAt: *device.RevokedAt,
		})
		return
	}
	writeV1Error(w, http.StatusInternalServerError, V1ErrorInternal, "The server could not read the revoked device.", nil)
}

func v1NativeDeviceFromModel(device models.Device) v1NativeDevice {
	return v1NativeDevice{
		ID:            device.PublicID,
		Name:          device.Name,
		Platform:      device.Platform,
		ClientVersion: device.ClientVersion,
		CreatedAt:     device.CreatedAt,
		LastSeenAt:    device.LastSeenAt,
		RevokedAt:     device.RevokedAt,
	}
}

func writeV1AuthorizationNotFound(w http.ResponseWriter) {
	writeV1Error(w, http.StatusNotFound, V1ErrorAuthorizationNotFound, "The device authorization was not found.", nil)
}

func setV1NoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}
