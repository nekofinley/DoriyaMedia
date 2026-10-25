package bangumi

import (
	"strings"
	"testing"
)

func TestTokenEncryptionRoundTrip(t *testing.T) {
	const secret = "server-secret"
	const token = "bgm-personal-token"

	enc, err := encryptToken(secret, 7, token)
	if err != nil {
		t.Fatalf("encryptToken: %v", err)
	}
	if strings.Contains(enc, token) {
		t.Fatal("encrypted value contains the plaintext token")
	}
	got, err := decryptToken(secret, 7, enc)
	if err != nil {
		t.Fatalf("decryptToken: %v", err)
	}
	if got != token {
		t.Fatalf("decrypted token = %q, want %q", got, token)
	}
	if _, err := decryptToken("different-secret", 7, enc); err == nil {
		t.Fatal("decryptToken with a different secret succeeded")
	}
	if _, err := decryptToken(secret, 8, enc); err == nil {
		t.Fatal("decryptToken with a different user ID succeeded")
	}
}
