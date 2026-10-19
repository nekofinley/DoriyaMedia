package netproxy

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// shadowsocks.go: a dialer that speaks Shadowsocks' AEAD protocol (SIP004 /
// SIP007) rather than SOCKS.
//
// Why a local implementation instead of pointing at a tunnel client: the whole
// point of the proxy here is that the server cannot reach bilibili, and it is
// the server that fetches the bytes. A dialer that speaks the cipher itself
// needs nothing installed beside the binary, and it has the same shape as the
// SOCKS5 and HTTP dialers beside it — one net.Conn carrying the target
// connection — so the HTTP client and the ffmpeg bridge above it never have to
// know which kind of proxy they are talking through.
//
// On the wire: a random salt, then AEAD-sealed chunks (2-byte length + tag,
// then payload + tag) under a per-connection subkey from HKDF-SHA1. Only the
// three AEAD ciphers every current client offers are supported; the legacy
// stream ciphers are refused by name rather than silently mis-decoded.

// ssCipher is one AEAD cipher a Shadowsocks server may be configured with.
type ssCipher struct {
	name    string
	keySize int
	newAEAD func(key []byte) (cipher.AEAD, error)
}

var ssCiphers = map[string]ssCipher{
	"aes-128-gcm":            {name: "aes-128-gcm", keySize: 16, newAEAD: aesGCM},
	"aes-256-gcm":            {name: "aes-256-gcm", keySize: 32, newAEAD: aesGCM},
	"chacha20-ietf-poly1305": {name: "chacha20-ietf-poly1305", keySize: 32, newAEAD: chacha20poly1305.New},
}

func aesGCM(key []byte) (cipher.AEAD, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

// ssCipherByName resolves a method name. Aliases are accepted because the
// method is copied out of a client's config file, and every client spells it a
// little differently.
func ssCipherByName(name string) (ssCipher, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "aes-256-gcm", "aead_aes_256_gcm":
		return ssCiphers["aes-256-gcm"], nil
	case "aes-128-gcm", "aead_aes_128_gcm":
		return ssCiphers["aes-128-gcm"], nil
	case "chacha20-ietf-poly1305", "aead_chacha20_poly1305", "chacha20-poly1305":
		return ssCiphers["chacha20-ietf-poly1305"], nil
	case "rc4-md5", "aes-128-cfb", "aes-192-cfb", "aes-256-cfb",
		"aes-128-ctr", "aes-192-ctr", "aes-256-ctr",
		"chacha20", "chacha20-ietf",
		"2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305":
		return ssCipher{}, fmt.Errorf("Shadowsocks 加密方式 %q 暂不支持(可用 aes-256-gcm / aes-128-gcm / chacha20-ietf-poly1305)", name)
	}
	return ssCipher{}, fmt.Errorf("未知的 Shadowsocks 加密方式 %q(可用 aes-256-gcm / aes-128-gcm / chacha20-ietf-poly1305)", name)
}

// ssKDF derives the master key from a password: MD5 in OpenSSL's
// EVP_BytesToKey shape, which every Shadowsocks server still uses.
func ssKDF(password string, keyLen int) []byte {
	var b, prev []byte
	h := md5.New()
	for len(b) < keyLen {
		h.Write(prev)
		h.Write([]byte(password))
		b = h.Sum(b)
		prev = b[len(b)-h.Size():]
		h.Reset()
	}
	return b[:keyLen]
}

// saltSize is the salt length for this cipher: its key size, but never less
// than 16 bytes.
func (c ssCipher) saltSize() int {
	if c.keySize > 16 {
		return c.keySize
	}
	return 16
}

// subkey is the per-connection AEAD key derived from one salt.
func (c ssCipher) subkey(password string, salt []byte) (cipher.AEAD, error) {
	sub := make([]byte, c.keySize)
	r := hkdf.New(sha1.New, ssKDF(password, c.keySize), salt, []byte("ss-subkey"))
	if _, err := io.ReadFull(r, sub); err != nil {
		return nil, err
	}
	return c.newAEAD(sub)
}

// ssDialer carries each connection through one Shadowsocks server. It holds no
// per-connection state: everything lives on the returned ssConn.
type ssDialer struct {
	server   string // host:port
	password string
	cipher   ssCipher
	timeout  time.Duration
}

// DialContext opens a TCP connection to the server and sends it the sealed
// address header that asks for target.
func (d *ssDialer) DialContext(ctx context.Context, network, target string) (net.Conn, error) {
	if !strings.HasPrefix(network, "tcp") {
		return nil, fmt.Errorf("Shadowsocks 只支持 TCP(%s)", network)
	}
	var nd net.Dialer
	if d.timeout > 0 {
		nd.Timeout = d.timeout
	}
	conn, err := nd.DialContext(ctx, "tcp", d.server)
	if err != nil {
		return nil, fmt.Errorf("连不上 Shadowsocks 服务器 %s: %w", d.server, err)
	}
	sc, err := newSSConn(conn, d.cipher, d.password)
	if err != nil {
		conn.Close()
		return nil, err
	}
	header, err := ssAddressHeader(target)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := sc.Write(header); err != nil {
		conn.Close()
		return nil, fmt.Errorf("Shadowsocks 握手失败: %w", err)
	}
	return sc, nil
}

// ssConn is a net.Conn whose bytes are the AEAD-sealed Shadowsocks stream.
type ssConn struct {
	net.Conn
	cipher   ssCipher
	password string

	wAEAD  cipher.AEAD
	wNonce []byte
	rAEAD  cipher.AEAD
	rNonce []byte

	buf      []byte
	leftover []byte
}

// newSSConn sends the write salt and prepares the writer. The reader's salt is
// the server's first bytes and is read lazily, so a server that never answers
// cannot block a dial.
func newSSConn(conn net.Conn, c ssCipher, password string) (*ssConn, error) {
	salt := make([]byte, c.saltSize())
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	aead, err := c.subkey(password, salt)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(salt); err != nil {
		return nil, err
	}
	return &ssConn{
		Conn:     conn,
		cipher:   c,
		password: password,
		wAEAD:    aead,
		wNonce:   make([]byte, aead.NonceSize()),
	}, nil
}

// ssMaxPayload is the largest payload one chunk may carry: the length field is
// a 14-bit value.
const ssMaxPayload = 0x3FFF

func (c *ssConn) Write(b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		n := len(b)
		if n > ssMaxPayload {
			n = ssMaxPayload
		}
		var lenBuf [2]byte
		binary.BigEndian.PutUint16(lenBuf[:], uint16(n))
		head := c.wAEAD.Seal(nil, c.wNonce, lenBuf[:], nil)
		incrementNonce(c.wNonce)
		body := c.wAEAD.Seal(nil, c.wNonce, b[:n], nil)
		incrementNonce(c.wNonce)
		if _, err := c.Conn.Write(head); err != nil {
			return total, err
		}
		if _, err := c.Conn.Write(body); err != nil {
			return total, err
		}
		total += n
		b = b[n:]
	}
	return total, nil
}

func (c *ssConn) Read(b []byte) (int, error) {
	if len(c.leftover) > 0 {
		n := copy(b, c.leftover)
		c.leftover = c.leftover[n:]
		return n, nil
	}
	if c.rAEAD == nil {
		salt := make([]byte, c.cipher.saltSize())
		if _, err := io.ReadFull(c.Conn, salt); err != nil {
			return 0, err
		}
		aead, err := c.cipher.subkey(c.password, salt)
		if err != nil {
			return 0, err
		}
		c.rAEAD = aead
		c.rNonce = make([]byte, aead.NonceSize())
	}
	size, err := c.readChunk()
	if err != nil {
		return 0, err
	}
	n := copy(b, c.buf[:size])
	if n < size {
		c.leftover = c.buf[n:size]
	}
	return n, nil
}

// readChunk decrypts one chunk into c.buf and returns its length.
func (c *ssConn) readChunk() (int, error) {
	head := make([]byte, 2+c.rAEAD.Overhead())
	if _, err := io.ReadFull(c.Conn, head); err != nil {
		return 0, err
	}
	if _, err := c.rAEAD.Open(head[:0], c.rNonce, head, nil); err != nil {
		return 0, fmt.Errorf("Shadowsocks 数据解密失败(密码或加密方式不对): %w", err)
	}
	incrementNonce(c.rNonce)
	size := int(binary.BigEndian.Uint16(head[:2])) & ssMaxPayload

	body := make([]byte, size+c.rAEAD.Overhead())
	if _, err := io.ReadFull(c.Conn, body); err != nil {
		return 0, err
	}
	if _, err := c.rAEAD.Open(body[:0], c.rNonce, body, nil); err != nil {
		return 0, fmt.Errorf("Shadowsocks 数据解密失败(密码或加密方式不对): %w", err)
	}
	incrementNonce(c.rNonce)
	if cap(c.buf) < size {
		c.buf = make([]byte, size)
	}
	c.buf = c.buf[:size]
	copy(c.buf, body[:size])
	return size, nil
}

// ssAddressHeader is the address a Shadowsocks server expects first: the
// SOCKS5 ATYP form.
func ssAddressHeader(target string) ([]byte, error) {
	host, portTxt, err := net.SplitHostPort(target)
	if err != nil {
		return nil, fmt.Errorf("Shadowsocks 目标地址无效 %q: %w", target, err)
	}
	port, err := strconv.Atoi(portTxt)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("Shadowsocks 目标端口无效 %q", portTxt)
	}
	var b []byte
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			b = append([]byte{1}, v4...)
		} else {
			b = append([]byte{4}, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return nil, fmt.Errorf("Shadowsocks 域名过长: %q", host)
		}
		b = append([]byte{3, byte(len(host))}, host...)
	}
	return append(b, byte(port>>8), byte(port)), nil
}

// incrementNonce bumps the little-endian counter both directions keep.
func incrementNonce(b []byte) {
	for i := range b {
		b[i]++
		if b[i] != 0 {
			return
		}
	}
}
