package bangumi

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const encryptedPrefix = "v1:"

// A per-install key derived from config.json's secret keeps personal access
// tokens out of plaintext SQLite files and backups. The secret is already
// required for signed media URLs, so no new operator setup is introduced.
func encryptToken(secret string, userID int64, token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("令牌为空")
	}
	block, err := aes.NewCipher(tokenKey(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(token), tokenAAD(userID))
	return encryptedPrefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

func decryptToken(secret string, userID int64, encoded string) (string, error) {
	if !strings.HasPrefix(encoded, encryptedPrefix) {
		return "", fmt.Errorf("令牌存储格式未知")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(encoded, encryptedPrefix))
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(tokenKey(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("令牌存储数据不完整")
	}
	nonce, sealed := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, sealed, tokenAAD(userID))
	if err != nil {
		return "", fmt.Errorf("无法解密 Bangumi 令牌(服务器 secret 是否变更?)")
	}
	return string(plain), nil
}

// Binding the ciphertext to the Doriya user prevents a copied database row from
// being reassigned to another account.
func tokenAAD(userID int64) []byte {
	return []byte("doriyamedia:bangumi:" + strconv.FormatInt(userID, 10))
}

func tokenKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}
