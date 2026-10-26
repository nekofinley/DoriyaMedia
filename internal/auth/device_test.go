package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDeviceCredentialTTLs(t *testing.T) {
	if DeviceAuthorizationTTL != 10*time.Minute {
		t.Fatalf("authorization TTL = %v, want 10m", DeviceAuthorizationTTL)
	}
	if DeviceAccessTokenTTL != 15*time.Minute {
		t.Fatalf("access TTL = %v, want 15m", DeviceAccessTokenTTL)
	}
	if DeviceRefreshTokenTTL != 30*24*time.Hour {
		t.Fatalf("refresh TTL = %v, want 30d", DeviceRefreshTokenTTL)
	}
	if DeviceRefreshTokenFamilyTTL != 180*24*time.Hour {
		t.Fatalf("refresh family TTL = %v, want 180d", DeviceRefreshTokenFamilyTTL)
	}
	if DeviceAuthorizationCSRFTTL != 5*time.Minute {
		t.Fatalf("authorization CSRF TTL = %v, want 5m", DeviceAuthorizationCSRFTTL)
	}
}

func TestDeviceUserCodeNormalization(t *testing.T) {
	got, err := NormalizeDeviceUserCode(" abcoI-l2x9z ")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got != "ABC01-12X9Z" {
		t.Fatalf("normalized code = %q, want ABC01-12X9Z", got)
	}

	for _, input := range []string{"", "short", "ABCDE-FGHUJ", "ABCDE-FGH!K", "ABCDE-FGHUK"} {
		if _, err := NormalizeDeviceUserCode(input); !errors.Is(err, ErrInvalidDeviceUserCode) {
			t.Errorf("NormalizeDeviceUserCode(%q) error = %v, want invalid", input, err)
		}
	}
}

func TestNewDeviceUserCodeShape(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 64; i++ {
		code, err := NewDeviceUserCode()
		if err != nil {
			t.Fatalf("new user code: %v", err)
		}
		if len(code) != 11 || code[5] != '-' {
			t.Fatalf("user code %q does not have XXXXX-XXXXX shape", code)
		}
		if seen[code] {
			t.Fatalf("duplicate user code generated: %q", code)
		}
		seen[code] = true
		if normalized, err := NormalizeDeviceUserCode(strings.ToLower(code)); err != nil || normalized != code {
			t.Fatalf("generated code %q did not round-trip: %q, %v", code, normalized, err)
		}
	}
}

func TestPKCES256RFCVector(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"

	got, err := PKCEChallengeS256(verifier)
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	if got != challenge {
		t.Fatalf("challenge = %q, want %q", got, challenge)
	}
	if !VerifyPKCE(verifier, challenge) {
		t.Fatal("valid RFC verifier/challenge pair was rejected")
	}
	if VerifyPKCE(verifier+"x", challenge) {
		t.Fatal("wrong verifier was accepted")
	}
}

func TestPKCERejectsPlainAndMalformedValues(t *testing.T) {
	verifier, err := NewPKCEVerifier()
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	challenge, err := PKCEChallengeS256(verifier)
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	if VerifyPKCE(verifier, verifier) {
		t.Fatal("plain verifier was accepted as a challenge")
	}
	if VerifyPKCE("too-short", challenge) {
		t.Fatal("short verifier was accepted")
	}
	if ValidatePKCEChallenge("plain") {
		t.Fatal("plain challenge was accepted")
	}
	if !ValidatePKCEChallenge(challenge) {
		t.Fatalf("generated S256 challenge %q was rejected", challenge)
	}
	if ValidatePKCEVerifier(strings.Repeat("x", PKCEVerifierMinLength-1)) == nil {
		t.Fatal("short PKCE verifier passed validation")
	}
	if ValidatePKCEVerifier(strings.Repeat("x", PKCEVerifierMaxLength+1)) == nil {
		t.Fatal("long PKCE verifier passed validation")
	}
	if ValidatePKCEVerifier(strings.Repeat("x", 42)+"!") == nil {
		t.Fatal("PKCE verifier with a disallowed character passed validation")
	}
}

func TestDeviceCredentialGenerationAndHashing(t *testing.T) {
	generators := []struct {
		name   string
		prefix string
		new    func() (string, error)
	}{
		{"device code", "dc_", NewDeviceCode},
		{"access token", "at_", NewDeviceAccessToken},
		{"refresh token", "rt_", NewDeviceRefreshToken},
		{"device id", "dev_", NewDevicePublicID},
		{"authorization id", "dva_", NewDeviceAuthorizationID},
		{"family id", "fam_", NewDeviceTokenFamilyID},
		{"authorization csrf", "csrf_", NewDeviceAuthorizationCSRFToken},
	}
	for _, tc := range generators {
		t.Run(tc.name, func(t *testing.T) {
			first, err := tc.new()
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			second, err := tc.new()
			if err != nil {
				t.Fatalf("generate second: %v", err)
			}
			if !strings.HasPrefix(first, tc.prefix) || len(first) <= len(tc.prefix) {
				t.Fatalf("generated value %q lacks prefix %q", first, tc.prefix)
			}
			if first == second {
				t.Fatalf("two generated values are identical: %q", first)
			}
			hash := HashDeviceCredential(first)
			if len(hash) != 64 || hash == first {
				t.Fatalf("unexpected credential hash %q", hash)
			}
		})
	}
}
