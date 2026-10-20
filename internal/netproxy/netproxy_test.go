package netproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ---------- region classification ----------

// A proxy in Hong Kong is not an error: it unlocks the 港澳台 catalogue while
// the mainland-only titles answer -10403. The two flags drive the settings
// page's colour and message, so they must not both be false, and a mainland
// address must never be reported as HMT (or the other way round).
func TestZoneClassification(t *testing.T) {
	cases := []struct {
		country, region string
		mainland, hmt   bool
	}{
		{"中国", "广东深圳", true, false},
		{"CN", "Zhejiang", true, false},
		{"中国", "香港", false, true},
		{"中国", "澳门", false, true},
		{"中国", "台湾", false, true},
		{"中国台湾", "", false, true},
		{"Hong Kong", "", false, true},
		{"日本", "东京", false, false},
		{"美国", "加利福尼亚", false, false},
	}
	for _, c := range cases {
		if got := isMainland(c.country, c.region); got != c.mainland {
			t.Errorf("isMainland(%q, %q) = %v, want %v", c.country, c.region, got, c.mainland)
		}
		if got := isHMT(c.country, c.region); got != c.hmt {
			t.Errorf("isHMT(%q, %q) = %v, want %v", c.country, c.region, got, c.hmt)
		}
	}
}

// ---------- parsing ----------

func TestParseAcceptsTheFormsUsersPaste(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		scheme   string
		host     string
		user     string
		pass     string
		method   string
		password string
	}{
		{name: "socks5", raw: "socks5://127.0.0.1:1080", scheme: SchemeSocks5, host: "127.0.0.1:1080"},
		{name: "socks5 with auth", raw: "socks5://alice:s3cret@10.0.0.2:1080", scheme: SchemeSocks5, host: "10.0.0.2:1080", user: "alice", pass: "s3cret"},
		{name: "socks5h is the same", raw: "socks5h://10.0.0.2:1080", scheme: SchemeSocks5, host: "10.0.0.2:1080"},
		{name: "http", raw: "http://192.168.1.9:3128", scheme: SchemeHTTP, host: "192.168.1.9:3128"},
		{name: "bare host:port is socks5", raw: "127.0.0.1:7890", scheme: SchemeSocks5, host: "127.0.0.1:7890"},
		{
			name: "ss plain user info", raw: "ss://aes-256-gcm:pass%40word@example.com:8388",
			scheme: SchemeSS, host: "example.com:8388", method: "aes-256-gcm", password: "pass@word",
		},
		{
			name:     "ss base64 user info",
			raw:      "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:abc")) + "@example.com:8388",
			scheme:   SchemeSS,
			host:     "example.com:8388",
			method:   "aes-128-gcm",
			password: "abc",
		},
		{
			name:     "ss fully encoded",
			raw:      "ss://" + base64.RawURLEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:pw@1.2.3.4:8388")),
			scheme:   SchemeSS,
			host:     "1.2.3.4:8388",
			method:   "chacha20-ietf-poly1305",
			password: "pw",
		},
		{
			name:     "ss with a node-name fragment",
			raw:      "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:pw")) + "@example.com:8388#%E9%A6%99%E6%B8%AF%E8%8A%82%E7%82%B9",
			scheme:   SchemeSS,
			host:     "example.com:8388",
			method:   "aes-256-gcm",
			password: "pw",
		},
		{
			name:     "ss client json",
			raw:      `{"server":"example.com","server_port":8388,"method":"aes-256-gcm","password":"pw"}`,
			scheme:   SchemeSS,
			host:     "example.com:8388",
			method:   "aes-256-gcm",
			password: "pw",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.raw, err)
			}
			if spec.Scheme != tc.scheme {
				t.Fatalf("scheme = %q, want %q", spec.Scheme, tc.scheme)
			}
			if spec.Host != tc.host {
				t.Fatalf("host = %q, want %q", spec.Host, tc.host)
			}
			if spec.User != tc.user || spec.Pass != tc.pass {
				t.Fatalf("auth = %q/%q, want %q/%q", spec.User, spec.Pass, tc.user, tc.pass)
			}
			if spec.Method != tc.method || spec.Password != tc.password {
				t.Fatalf("ss = %q/%q, want %q/%q", spec.Method, spec.Password, tc.method, tc.password)
			}
		})
	}
}

func TestParseRefusesWhatCannotWork(t *testing.T) {
	cases := []string{
		"socks5://",                       // no host
		"ss://aes-256-gcm:pw@example.com", // no port
		"ss://!!!not-base64!!!",           // unparseable
		"ftp://example.com:21",            // unknown scheme
		"ss://aes-256-gcm:pw@example.com:8388?plugin=v2ray-plugin;tls", // plugin
	}
	for _, raw := range cases {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("Parse(%q) accepted a setting that cannot work", raw)
		}
	}
}

func TestParseEmptyIsNoProxy(t *testing.T) {
	if _, err := Parse("   "); err != ErrNoProxy {
		t.Fatalf("empty proxy: err = %v, want ErrNoProxy", err)
	}
}

// Display must never leak the secret: it is rendered on a page.
func TestDisplayMasksSecrets(t *testing.T) {
	for _, raw := range []string{
		"ss://aes-256-gcm:supersecret@example.com:8388",
		"socks5://alice:supersecret@127.0.0.1:1080",
		"http://alice:supersecret@127.0.0.1:3128",
	} {
		spec, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		shown := spec.Display()
		if strings.Contains(shown, "supersecret") {
			t.Fatalf("Display() leaked the password: %q", shown)
		}
		if shown == "" {
			t.Fatalf("Display() is empty for %q", raw)
		}
	}
}

func TestUnsupportedCiphersAreNamed(t *testing.T) {
	for _, method := range []string{"rc4-md5", "aes-256-cfb", "chacha20-ietf", "2022-blake3-aes-256-gcm"} {
		if _, err := ssCipherByName(method); err == nil {
			t.Fatalf("cipher %q accepted; legacy/2022 ciphers are not implemented", method)
		}
	}
	for _, method := range []string{"aes-256-gcm", "aes-128-gcm", "chacha20-ietf-poly1305", ""} {
		if _, err := ssCipherByName(method); err != nil {
			t.Fatalf("cipher %q refused: %v", method, err)
		}
	}
}

// ---------- Shadowsocks wire format ----------

// TestShadowsocksRoundTrip drives the client against a server built from the
// same primitives, and checks the bytes that come back are the bytes sent. It
// is the one test that proves the framing (salt, length chunk, payload chunk,
// nonce counting) is right in both directions.
func TestShadowsocksRoundTrip(t *testing.T) {
	cipher, err := ssCipherByName("aes-256-gcm")
	if err != nil {
		t.Fatal(err)
	}
	password := "test-password"

	// A "server": accept, read the address header, answer with a known payload.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	want := make([]byte, 40000) // larger than one chunk, so framing is exercised
	if _, err := io.ReadFull(rand.Reader, want); err != nil {
		t.Fatal(err)
	}

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Read the client's salt, then its address header.
		salt := make([]byte, cipher.saltSize())
		if _, err := io.ReadFull(conn, salt); err != nil {
			return
		}
		aead, err := cipher.subkey(password, salt)
		if err != nil {
			return
		}
		nonce := make([]byte, aead.NonceSize())
		header, err := readSSChunk(conn, aead, nonce)
		if err != nil {
			return
		}
		_ = header // the address; this fake server ignores it

		// Answer with a fresh salt and the payload.
		outSalt := make([]byte, cipher.saltSize())
		io.ReadFull(rand.Reader, outSalt)
		outAEAD, err := cipher.subkey(password, outSalt)
		if err != nil {
			return
		}
		conn.Write(outSalt)
		writeSSChunks(conn, outAEAD, make([]byte, outAEAD.NonceSize()), want)
	}()

	d := &ssDialer{server: ln.Addr().String(), password: password, cipher: cipher, timeout: 5 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", "example.com:80")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Fatal("payload did not survive the Shadowsocks round trip")
	}
}

// readSSChunk decrypts one chunk (length then payload) from a server stream.
func readSSChunk(r io.Reader, aead interface {
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
	Overhead() int
	NonceSize() int
}, nonce []byte) ([]byte, error) {
	head := make([]byte, 2+aead.Overhead())
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, err
	}
	if _, err := aead.Open(head[:0], nonce, head, nil); err != nil {
		return nil, err
	}
	incrementNonce(nonce)
	size := int(head[0])<<8 | int(head[1])
	size &= ssMaxPayload
	body := make([]byte, size+aead.Overhead())
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	if _, err := aead.Open(body[:0], nonce, body, nil); err != nil {
		return nil, err
	}
	incrementNonce(nonce)
	return body[:size], nil
}

// writeSSChunks seals payload into chunks, the way a server would.
func writeSSChunks(w io.Writer, aead interface {
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Overhead() int
	NonceSize() int
}, nonce []byte, payload []byte) error {
	for len(payload) > 0 {
		n := len(payload)
		if n > ssMaxPayload {
			n = ssMaxPayload
		}
		var lenBuf [2]byte
		lenBuf[0], lenBuf[1] = byte(n>>8), byte(n)
		head := aead.Seal(nil, nonce, lenBuf[:], nil)
		incrementNonce(nonce)
		body := aead.Seal(nil, nonce, payload[:n], nil)
		incrementNonce(nonce)
		if _, err := w.Write(head); err != nil {
			return err
		}
		if _, err := w.Write(body); err != nil {
			return err
		}
		payload = payload[n:]
	}
	return nil
}

func TestSSAddressHeaderShapes(t *testing.T) {
	cases := []struct {
		target string
		want   []byte
	}{
		{"1.2.3.4:80", []byte{1, 1, 2, 3, 4, 0, 80}},
		{"example.com:8388", append([]byte{3, 11}, append([]byte("example.com"), 0x20, 0xc4)...)},
	}
	for _, tc := range cases {
		got, err := ssAddressHeader(tc.target)
		if err != nil {
			t.Fatalf("ssAddressHeader(%q): %v", tc.target, err)
		}
		if string(got) != string(tc.want) {
			t.Fatalf("ssAddressHeader(%q) = %v, want %v", tc.target, got, tc.want)
		}
	}
}

func TestSSKDFMatchesTheSpec(t *testing.T) {
	// EVP_BytesToKey(MD5) with "foobar" and a 32-byte key: this is the vector
	// every Shadowsocks implementation agrees on.
	got := ssKDF("foobar", 32)
	// EVP_BytesToKey(MD5, no salt, 1 iteration) of "foobar", 32 bytes. Computed
	// independently with hashlib so the constant is not this code's own output.
	want := []byte{
		0x38, 0x58, 0xf6, 0x22, 0x30, 0xac, 0x3c, 0x91,
		0x5f, 0x30, 0x0c, 0x66, 0x43, 0x12, 0xc6, 0x3f,
		0x56, 0x83, 0x78, 0x52, 0x96, 0x14, 0xd2, 0x2d,
		0xdb, 0x49, 0x23, 0x7d, 0x2f, 0x60, 0xbf, 0xdf,
	}
	if string(got) != string(want) {
		t.Fatalf("ssKDF mismatch:\n got %x\nwant %x", got, want)
	}
}

// ---------- the bridge ffmpeg uses ----------

// TestBridgeTunnelsThroughSOCKS5 proves the piece ffmpeg depends on: a CONNECT
// request against the loopback listener reaches the target through the real
// proxy. A minimal SOCKS5 server stands in for the tunnel client.
func TestBridgeTunnelsThroughSOCKS5(t *testing.T) {
	socksAddr, stop := startFakeSOCKS5(t, "hello-through-the-proxy")
	defer stop()

	spec, err := Parse("socks5://" + socksAddr)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BridgeFor(spec)
	if err != nil {
		t.Fatalf("BridgeFor: %v", err)
	}

	// Speak to the bridge exactly as ffmpeg does: CONNECT, then read the tunnel.
	conn, err := net.Dial("tcp", b.Addr())
	if err != nil {
		t.Fatalf("dial bridge: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT example.test:80 HTTP/1.1\r\nHost: example.test:80\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for !strings.Contains(string(buf), "\r\n\r\n") {
		n, err := conn.Read(tmp)
		if err != nil {
			t.Fatalf("read CONNECT reply: %v (got %q)", err, buf)
		}
		buf = append(buf, tmp[:n]...)
	}
	if !strings.Contains(string(buf), "200") {
		t.Fatalf("bridge refused CONNECT: %q", buf)
	}
	payload := make([]byte, len("hello-through-the-proxy"))
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatalf("read through the tunnel: %v", err)
	}
	if string(payload) != "hello-through-the-proxy" {
		t.Fatalf("tunnel payload = %q", payload)
	}
}

// TestBridgeRefusesUnknownRequest keeps the bridge honest: it is a proxy for
// the fetches it was configured for, not an open relay. An origin-form request
// addressed at the listener itself is refused.
func TestBridgeRefusesUnknownRequest(t *testing.T) {
	socksAddr, stop := startFakeSOCKS5(t, "")
	defer stop()
	spec, _ := Parse("socks5://" + socksAddr)
	b, err := BridgeFor(spec)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", b.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET /nothing HTTP/1.1\r\nHost: bridge\r\n\r\n")
	reply := make([]byte, 128)
	n, _ := conn.Read(reply)
	if !strings.Contains(string(reply[:n]), "405") {
		t.Fatalf("bridge answered an origin-form request with %q, want 405", reply[:n])
	}
}

// TestBridgeForwardsPlainHTTP covers the other half of what ffmpeg does through
// the bridge: for an http:// input it sends an absolute-URI request instead of a
// CONNECT, and bilibili's DASH URLs are often http. The payload must arrive
// through the proxy, ranges included.
func TestBridgeForwardsPlainHTTP(t *testing.T) {
	body := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "media.m4s", time.Now(), bytes.NewReader(body))
	}))
	defer origin.Close()

	socksAddr, stop := startForwardingSOCKS5(t)
	defer stop()

	spec, err := Parse("socks5://" + socksAddr)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BridgeFor(spec)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := url.Parse(b.URL())
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}

	resp, err := client.Get(origin.URL + "/media.m4s")
	if err != nil {
		t.Fatalf("GET through the bridge: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != string(body) {
		t.Fatalf("body through the bridge = %q, want %q", got, body)
	}

	// A range request is what seeking turns into; the 206 must survive.
	req, _ := http.NewRequest(http.MethodGet, origin.URL+"/media.m4s", nil)
	req.Header.Set("Range", "bytes=10-19")
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatalf("range request through the bridge: %v", err)
	}
	got2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", resp2.StatusCode)
	}
	if string(got2) != string(body[10:20]) {
		t.Fatalf("range body = %q, want %q", got2, body[10:20])
	}
}

// startForwardingSOCKS5 runs a SOCKS5 server that connects to whatever target it
// is asked for, so a forward request can be checked end to end.
func startForwardingSOCKS5(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(done)
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				head := make([]byte, 2)
				if _, err := io.ReadFull(c, head); err != nil {
					return
				}
				io.ReadFull(c, make([]byte, int(head[1])))
				c.Write([]byte{5, 0})
				req := make([]byte, 4)
				if _, err := io.ReadFull(c, req); err != nil {
					return
				}
				var host string
				switch req[3] {
				case 1:
					b := make([]byte, 4)
					io.ReadFull(c, b)
					host = net.IP(b).String()
				case 3:
					l := make([]byte, 1)
					io.ReadFull(c, l)
					b := make([]byte, int(l[0]))
					io.ReadFull(c, b)
					host = string(b)
				case 4:
					b := make([]byte, 16)
					io.ReadFull(c, b)
					host = net.IP(b).String()
				}
				p := make([]byte, 2)
				io.ReadFull(c, p)
				target := net.JoinHostPort(host, fmt.Sprint(int(p[0])<<8|int(p[1])))
				up, err := net.Dial("tcp", target)
				if err != nil {
					c.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				go io.Copy(up, c)
				io.Copy(c, up)
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close(); <-done }
}

// startFakeSOCKS5 runs a SOCKS5 server that accepts any CONNECT and then writes
// payload, so a dialer's tunnel can be checked end to end.
func startFakeSOCKS5(t *testing.T, payload string) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(done)
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// greeting: version, nmethods, methods
				head := make([]byte, 2)
				if _, err := io.ReadFull(c, head); err != nil {
					return
				}
				methods := make([]byte, int(head[1]))
				io.ReadFull(c, methods)
				c.Write([]byte{5, 0}) // no auth
				// request: ver, cmd, rsv, atyp
				req := make([]byte, 4)
				if _, err := io.ReadFull(c, req); err != nil {
					return
				}
				switch req[3] {
				case 1:
					b := make([]byte, 4)
					io.ReadFull(c, b)
				case 3:
					l := make([]byte, 1)
					io.ReadFull(c, l)
					io.ReadFull(c, make([]byte, int(l[0])))
				case 4:
					io.ReadFull(c, make([]byte, 16))
				}
				port := make([]byte, 2)
				io.ReadFull(c, port)
				c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
				if payload != "" {
					c.Write([]byte(payload))
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close(); <-done }
}
