package netproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// netproxy.go: the proxy a source dials through, as one small vocabulary.
//
// A source that has to reach bilibili from a machine bilibili refuses to serve
// (its region check answers -10403 abroad) needs its own way out, separate from
// the proxies the scrapers use: the scraper's proxy is configured for TMDB and
// Bangumi and may be a completely different subscription. So a source carries
// its own proxy string, in the same syntax every client writes in its config,
// and this package turns that string into a dialer.
//
// Accepted forms (the box a user pastes from a client's config or a share link):
//
//	socks5://127.0.0.1:1080          SOCKS5, hostnames resolved by the proxy
//	socks5h://user:pass@host:1080    the same, with authentication
//	http://host:3128                 HTTP CONNECT (https:// is accepted too)
//	ss://method:password@host:port   Shadowsocks AEAD (SIP002, base64 or not)
//	ss://base64(method:password)@host:port
//	ss://base64(method:password@host:port)
//	host:port                        SOCKS5, for the bare form Clash prints
//
// A Shadowsocks client's "copy config" also yields a JSON blob and its share
// link, and both are unwrapped: a paste that carries `"server"`, `"server_port"`
// and `"method"` is read as Shadowsocks, and a `ss://` link with a fragment is
// read with the fragment ignored (it is the node's display name).
const (
	SchemeSocks5 = "socks5"
	SchemeHTTP   = "http"
	SchemeSS     = "ss"
)

// ErrNoProxy is returned by Parse for an empty string: no proxy is not an error,
// it is the ordinary case.
var ErrNoProxy = errors.New("no proxy configured")

// Spec is a parsed proxy setting.
type Spec struct {
	// Scheme is SchemeSocks5, SchemeHTTP or SchemeSS.
	Scheme string
	// Host is "host:port" of the proxy itself.
	Host string
	// User / Pass authenticate to the proxy (SOCKS5 and HTTP only).
	User, Pass string
	// Method / Password are Shadowsocks' cipher and shared secret.
	Method, Password string
	// Raw is what the user typed, kept for display and for round-tripping the
	// setting back into the form without rewriting it.
	Raw string
}

// Parse reads a proxy setting. The empty string yields ErrNoProxy, so a caller
// can tell "not configured" from "configured wrongly".
func Parse(raw string) (Spec, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return Spec{}, ErrNoProxy
	}
	// A client's "copy config" hands over JSON; unwrap it first.
	if strings.HasPrefix(text, "{") {
		return parseSSJSON(text)
	}
	if !strings.Contains(text, "://") {
		// The bare host:port form, taken as SOCKS5 — it is what the usual
		// Clash / v2rayN "local port" line looks like.
		host, port, err := net.SplitHostPort(text)
		if err != nil || host == "" || port == "" {
			return Spec{}, fmt.Errorf("代理地址无效: %q(示例 socks5://127.0.0.1:1080 或 ss://aes-256-gcm:密码@host:8388)", text)
		}
		return Spec{Scheme: SchemeSocks5, Host: net.JoinHostPort(host, port), Raw: text}, nil
	}

	u, err := url.Parse(text)
	if err != nil {
		return Spec{}, fmt.Errorf("代理地址无效: %q", text)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "socks", "socks5", "socks5h":
		if u.Host == "" {
			return Spec{}, fmt.Errorf("代理地址缺少主机: %q", text)
		}
		spec := Spec{Scheme: SchemeSocks5, Host: withDefaultPort(u.Host, "1080"), Raw: text}
		if u.User != nil {
			spec.User = u.User.Username()
			spec.Pass, _ = u.User.Password()
		}
		return spec, nil
	case "http", "https":
		if u.Host == "" {
			return Spec{}, fmt.Errorf("代理地址缺少主机: %q", text)
		}
		spec := Spec{Scheme: SchemeHTTP, Host: withDefaultPort(u.Host, "3128"), Raw: text}
		if u.User != nil {
			spec.User = u.User.Username()
			spec.Pass, _ = u.User.Password()
		}
		return spec, nil
	case "ss":
		return parseSSURI(text)
	}
	return Spec{}, fmt.Errorf("不支持的代理协议 %q(可用 socks5:// / http:// / ss://)", u.Scheme)
}

// withDefaultPort fills in the conventional port when the address has none.
func withDefaultPort(host, port string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		return host + ":" + port
	}
	if strings.Count(host, ":") > 1 {
		return "[" + host + "]:" + port // bare IPv6
	}
	return host + ":" + port
}

// Display is the setting as it may be shown: the password masked. The whole
// string is a credential (a Shadowsocks password is the key), so nothing here
// may reach a page that is not the admin's.
func (s Spec) Display() string {
	if s.Raw == "" {
		return ""
	}
	if s.Scheme != SchemeSS && s.Pass == "" {
		return s.Raw
	}
	u, err := url.Parse(s.Raw)
	if err != nil || u.User == nil {
		return s.Scheme + "://" + s.Host
	}
	if s.Scheme == SchemeSS {
		return s.Scheme + "://" + s.Method + ":******@" + s.Host
	}
	if _, hasPass := u.User.Password(); hasPass {
		u.User = url.UserPassword(u.User.Username(), "******")
	} else {
		u.User = url.User("******")
	}
	return u.String()
}

// Summary is one line for a status field: what this proxy is, without secrets.
func (s Spec) Summary() string {
	switch s.Scheme {
	case SchemeSS:
		return fmt.Sprintf("Shadowsocks %s @ %s", s.Method, s.Host)
	case SchemeHTTP:
		return "HTTP 代理 @ " + s.Host
	default:
		return "SOCKS5 @ " + s.Host
	}
}

// Dialer opens TCP connections through the proxy.
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// cacheKey identifies a proxy's settings, secrets included: two entries that
// differ only by password are two different proxies.
func (s Spec) cacheKey() string {
	return s.Scheme + "|" + s.Host + "|" + s.User + "|" + s.Pass + "|" + s.Method + "|" + s.Password
}

// NewDialer builds the dialer for a parsed spec. The same proxy string always
// yields a shared dialer, so a page that resolves one source repeatedly reuses
// its connections.
func NewDialer(spec Spec) (Dialer, error) {
	key := spec.cacheKey()
	dialersMu.Lock()
	defer dialersMu.Unlock()
	if d, ok := dialers[key]; ok {
		return d, nil
	}
	var d Dialer
	switch spec.Scheme {
	case SchemeSocks5:
		var auth *xproxy.Auth
		if spec.User != "" {
			auth = &xproxy.Auth{User: spec.User, Password: spec.Pass}
		}
		sd, err := xproxy.SOCKS5("tcp", spec.Host, auth, &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second})
		if err != nil {
			return nil, fmt.Errorf("SOCKS5 代理初始化失败: %w", err)
		}
		d = &socks5Dialer{inner: sd}
	case SchemeHTTP:
		d = &httpProxyDialer{addr: spec.Host, user: spec.User, pass: spec.Pass}
	case SchemeSS:
		cipher, err := ssCipherByName(spec.Method)
		if err != nil {
			return nil, err
		}
		d = &ssDialer{server: spec.Host, password: spec.Password, cipher: cipher, timeout: 20 * time.Second}
	default:
		return nil, fmt.Errorf("不支持的代理协议 %q", spec.Scheme)
	}
	dialers[key] = d
	return d, nil
}

var (
	dialersMu sync.Mutex
	dialers   = map[string]Dialer{}
)

// socks5Dialer adapts x/net/proxy's SOCKS5 dialer to DialContext. Its
// ContextDialer support is used when present; the fallback keeps cancellation
// working for the versions that do not implement it.
type socks5Dialer struct{ inner xproxy.Dialer }

func (d *socks5Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if cd, ok := d.inner.(xproxy.ContextDialer); ok {
		return cd.DialContext(ctx, network, addr)
	}
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := d.inner.Dial(network, addr)
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		return r.conn, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// httpProxyDialer tunnels through an HTTP proxy's CONNECT method. It is written
// by hand rather than with http.Transport: this dialer has to hand back a plain
// net.Conn, because what sits above it may be TLS the caller manages (ffmpeg
// over the bridge) or plain HTTP (the API calls).
type httpProxyDialer struct {
	addr string
	user string
	pass string
}

func (d *httpProxyDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if !strings.HasPrefix(network, "tcp") {
		return nil, fmt.Errorf("HTTP 代理只支持 TCP(%s)", network)
	}
	var nd net.Dialer
	conn, err := nd.DialContext(ctx, "tcp", d.addr)
	if err != nil {
		return nil, fmt.Errorf("连不上 HTTP 代理 %s: %w", d.addr, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
	if d.user != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(d.user + ":" + d.pass))
		b.WriteString("Proxy-Authorization: Basic " + cred + "\r\n")
	}
	b.WriteString("Proxy-Connection: keep-alive\r\n\r\n")
	if _, err := io.WriteString(conn, b.String()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("HTTP 代理握手失败: %w", err)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("HTTP 代理握手失败: %w", err)
	}
	if !strings.Contains(status, " 200") {
		conn.Close()
		return nil, fmt.Errorf("HTTP 代理拒绝连接 %s: %s", addr, strings.TrimSpace(status))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("HTTP 代理握手失败: %w", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		// Bytes the proxy already sent past its header belong to the tunnel.
		peek, _ := br.Peek(br.Buffered())
		return &bufferedConn{Conn: conn, pre: append([]byte(nil), peek...)}, nil
	}
	return conn, nil
}

// bufferedConn replays bytes read ahead while parsing a proxy's reply.
type bufferedConn struct {
	net.Conn
	pre []byte
}

func (c *bufferedConn) Read(b []byte) (int, error) {
	if len(c.pre) > 0 {
		n := copy(b, c.pre)
		c.pre = c.pre[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}

// NewClient builds an HTTP client that dials through the proxy. Every scheme is
// attached at the dialer (an HTTP proxy via its own CONNECT implementation), so
// hostnames are resolved on the proxy's side and a host the machine's own DNS
// cannot resolve still works.
func NewClient(spec Spec, timeout time.Duration) (*http.Client, error) {
	transport, err := NewTransport(spec)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

// NewTransport builds a transport dialing through the proxy.
func NewTransport(spec Spec) (*http.Transport, error) {
	d, err := NewDialer(spec)
	if err != nil {
		return nil, err
	}
	t := &http.Transport{
		DialContext:         d.DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 20 * time.Second,
		// A media stream can idle for minutes between seeks; a response header
		// timeout would abort a perfectly good connection.
		ResponseHeaderTimeout: 60 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return t, nil
}
