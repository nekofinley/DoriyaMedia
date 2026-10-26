package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"
)

const (
	// These TTLs are the contract for native clients. Keep them together so
	// handlers, tests and clients cannot drift on the security window.
	DeviceAuthorizationTTL = 10 * time.Minute
	DeviceAccessTokenTTL   = 15 * time.Minute
	DeviceRefreshTokenTTL  = 30 * 24 * time.Hour
	// A refresh family has an absolute lifetime even while each rotation keeps
	// extending the sliding 30-day window. This bounds the damage from a stolen
	// but repeatedly refreshed device credential.
	DeviceRefreshTokenFamilyTTL = 180 * 24 * time.Hour
	// The approval token is issued by lookup and consumed by the decision. It
	// only needs to survive the short review step on the authorization page.
	DeviceAuthorizationCSRFTTL = 5 * time.Minute

	PKCEMethodS256        = "S256"
	PKCEVerifierMinLength = 43
	PKCEVerifierMaxLength = 128

	deviceUserCodeLength = 10
)

var (
	ErrInvalidDeviceUserCode = errors.New("invalid device user code")
	ErrInvalidPKCEVerifier   = errors.New("invalid PKCE verifier")
	ErrInvalidPKCEChallenge  = errors.New("invalid PKCE S256 challenge")
)

// Crockford's base32 alphabet is case-insensitive and excludes I, L, O and U,
// which are the characters people most often mistype when copying a short code.
const deviceUserCodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NormalizeDeviceUserCode accepts the forms a person is likely to paste or
// type (lower case, spaces and hyphens) and returns the canonical
// XXXXX-XXXXX form. It also maps the common O/I/L confusions to 0/1. Callers
// must hash the canonical value, never keep the input as a lookup key.
func NormalizeDeviceUserCode(input string) (string, error) {
	var normalized strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(input)) {
		switch {
		case r == '-' || unicode.IsSpace(r):
			continue
		case r == 'O':
			r = '0'
		case r == 'I' || r == 'L':
			r = '1'
		}
		if !strings.ContainsRune(deviceUserCodeAlphabet, r) {
			return "", ErrInvalidDeviceUserCode
		}
		normalized.WriteRune(r)
	}
	if normalized.Len() != deviceUserCodeLength {
		return "", ErrInvalidDeviceUserCode
	}
	code := normalized.String()
	return code[:5] + "-" + code[5:], nil
}

// NewDeviceUserCode creates a 50-bit, human-entered code. The alphabet has 32
// symbols, so taking the low five bits of each random byte is unbiased and the
// short code still has substantially more entropy than a six-digit PIN.
func NewDeviceUserCode() (string, error) {
	raw := make([]byte, deviceUserCodeLength)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	var code strings.Builder
	code.Grow(len(raw) + 1)
	for i, b := range raw {
		if i == 5 {
			code.WriteByte('-')
		}
		code.WriteByte(deviceUserCodeAlphabet[int(b)&31])
	}
	return code.String(), nil
}

// NewPKCEVerifier returns an RFC 7636 verifier. The native client keeps this
// value private and sends only its S256 challenge to the server.
func NewPKCEVerifier() (string, error) {
	return randomCredential("", 32)
}

// PKCEChallengeS256 derives the only challenge method this server accepts.
func PKCEChallengeS256(verifier string) (string, error) {
	if err := ValidatePKCEVerifier(verifier); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// VerifyPKCE compares a verifier with a stored S256 challenge in constant
// time. Invalid verifier or challenge syntax fails closed.
func VerifyPKCE(verifier, challenge string) bool {
	if ValidatePKCEVerifier(verifier) != nil || !ValidatePKCEChallenge(challenge) {
		return false
	}
	got, err := PKCEChallengeS256(verifier)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

// ValidatePKCEVerifier enforces RFC 7636's 43..128 unreserved characters.
func ValidatePKCEVerifier(verifier string) error {
	if len(verifier) < PKCEVerifierMinLength || len(verifier) > PKCEVerifierMaxLength {
		return ErrInvalidPKCEVerifier
	}
	for i := 0; i < len(verifier); i++ {
		c := verifier[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '~' {
			continue
		}
		return ErrInvalidPKCEVerifier
	}
	return nil
}

// ValidatePKCEChallenge checks the exact base64url(SHA-256(verifier)) shape.
// There is deliberately no "plain" method: the device table also has a CHECK
// constraint so a plain challenge cannot be stored through another path.
func ValidatePKCEChallenge(challenge string) bool {
	if len(challenge) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(challenge)
	return err == nil && len(decoded) == sha256.Size
}

// HashDeviceCredential hashes high-entropy bearer credentials before they hit
// the database. The raw values are not recoverable from the stored digest.
func HashDeviceCredential(credential string) string {
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:])
}

func NewDeviceCode() (string, error) {
	return randomCredential("dc_", 32)
}

func NewDeviceAccessToken() (string, error) {
	return randomCredential("at_", 32)
}

func NewDeviceRefreshToken() (string, error) {
	return randomCredential("rt_", 32)
}

func NewDevicePublicID() (string, error) {
	return randomCredential("dev_", 18)
}

func NewDeviceAuthorizationID() (string, error) {
	return randomCredential("dva_", 18)
}

func NewDeviceTokenFamilyID() (string, error) {
	return randomCredential("fam_", 18)
}

// NewDeviceAuthorizationCSRFToken creates a one-time approval capability for
// the browser decision endpoint. Only its hash is stored.
func NewDeviceAuthorizationCSRFToken() (string, error) {
	return randomCredential("csrf_", 32)
}

func randomCredential(prefix string, byteLen int) (string, error) {
	raw := make([]byte, byteLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}
