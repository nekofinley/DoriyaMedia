package netproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// bridge.go: the piece that lets ffmpeg go through a proxy it cannot speak.
//
// Everything else in this server fetches through a net/http client, and a
// dialer is enough there. ffmpeg is not: it has one proxy option,
// `-http_proxy`, and it only honours it when the value starts with "http://"
// (measured against the 8.0 build in use: a socks5:// value is ignored and the
// CDN is dialled directly, which is exactly the request bilibili's region check
// refuses). So a SOCKS5 or Shadowsocks source cannot be handed to ffmpeg
// directly.
//
// The bridge closes that gap without teaching ffmpeg anything: it listens on
// 127.0.0.1 on a port the kernel picked, speaks HTTP CONNECT (the one proxy
// dialect ffmpeg does implement), and tunnels every connection through the real
// proxy. ffmpeg is then given `-http_proxy http://127.0.0.1:PORT`, and whether
// the far side is SOCKS5, Shadowsocks or another HTTP proxy makes no difference
// to it.
//
// The listener is created once per proxy for the process, bound to the loopback
// interface only, and retired after a spell with no connections so a server that
// never plays through a proxy does not hold a port open.

// bridgeIdleTimeout is how long an unused bridge listener lives. Packaging
// sessions come and go in bursts; a few minutes covers the pause between two
// episodes without keeping a port around for the rest of the process's life.
const bridgeIdleTimeout = 10 * time.Minute

// Bridge is one loopback CONNECT proxy forwarding to a real proxy.
type Bridge struct {
	addr   string
	ln     net.Listener
	dialer Dialer

	mu       sync.Mutex
	active   int
	lastUsed time.Time
	closed   bool
}

var (
	bridgesMu sync.Mutex
	bridges   = map[string]*Bridge{}
)

// BridgeFor returns a running loopback bridge for the spec, starting one if
// needed. The caller must not close it: bridges are shared and retire
// themselves.
func BridgeFor(spec Spec) (*Bridge, error) {
	key := spec.cacheKey()
	bridgesMu.Lock()
	defer bridgesMu.Unlock()
	if b, ok := bridges[key]; ok {
		b.mu.Lock()
		if !b.closed {
			b.lastUsed = time.Now()
			b.mu.Unlock()
			return b, nil
		}
		b.mu.Unlock()
		delete(bridges, key)
	}
	d, err := NewDialer(spec)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("代理桥监听失败: %w", err)
	}
	b := &Bridge{addr: ln.Addr().String(), ln: ln, dialer: d, lastUsed: time.Now()}
	bridges[key] = b
	go b.serve()
	return b, nil
}

// retireIdle closes bridges nobody has used for bridgeIdleTimeout. It takes
// bridgesMu itself, so it must be called with that lock released.
func retireIdle() {
	bridgesMu.Lock()
	defer bridgesMu.Unlock()
	now := time.Now()
	for key, b := range bridges {
		b.mu.Lock()
		if b.active == 0 && now.Sub(b.lastUsed) > bridgeIdleTimeout {
			b.closed = true
			_ = b.ln.Close()
			delete(bridges, key)
		}
		b.mu.Unlock()
	}
}

// URL is the value to hand ffmpeg as its proxy.
func (b *Bridge) URL() string { return "http://" + b.addr }

// Addr is the host:port ffmpeg dials.
func (b *Bridge) Addr() string { return b.addr }

func (b *Bridge) serve() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			b.mu.Lock()
			b.closed = true
			b.mu.Unlock()
			return
		}
		b.mu.Lock()
		b.active++
		b.lastUsed = time.Now()
		b.mu.Unlock()
		go b.handle(conn)
	}
}

// handle serves one client connection: a CONNECT tunnel, or a plain forward
// request — ffmpeg uses the first for https and the second for http, and the
// CDN hands out both, so both are needed.
func (b *Bridge) handle(client net.Conn) {
	defer func() {
		client.Close()
		b.mu.Lock()
		b.active--
		b.lastUsed = time.Now()
		b.mu.Unlock()
	}()

	_ = client.SetReadDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(client)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method == http.MethodConnect {
		b.tunnel(client, br, req)
		return
	}
	if req.URL != nil && req.URL.IsAbs() {
		b.forward(client, req)
		return
	}
	// Origin-form addressed at the bridge itself: not something ffmpeg sends.
	io.WriteString(client, "HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\n\r\n")
}

// tunnel answers a CONNECT and then gets out of the way.
func (b *Bridge) tunnel(client net.Conn, br *bufio.Reader, req *http.Request) {
	target := req.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	upstream, err := b.dialer.DialContext(ctx, "tcp", target)
	cancel()
	if err != nil {
		io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer upstream.Close()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	_ = client.SetReadDeadline(time.Time{})

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, br)
		if c, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		if c, ok := client.(*net.TCPConn); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	// Give the other direction a moment to drain, then close both: a CDN that
	// never closes must not pin the goroutine.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

// forward answers one plain-HTTP proxy request: ffmpeg sends "GET http://…"
// to the proxy rather than a CONNECT whenever the input is http, and bilibili's
// DASH URLs are frequently http — so refusing here would break exactly the
// sources this bridge exists for.
//
// The request is rewritten to origin form and sent over a connection dialled
// through the proxy; the reply is copied back as it arrives, which keeps range
// requests and their 206 answers working. The connection is closed afterwards
// (`Connection: close`), so a player that wants another range reconnects.
func (b *Bridge) forward(client net.Conn, req *http.Request) {
	target := req.URL.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		port := "80"
		if strings.EqualFold(req.URL.Scheme, "https") {
			port = "443"
		}
		target = net.JoinHostPort(target, port)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	upstream, err := b.dialer.DialContext(ctx, "tcp", target)
	cancel()
	if err != nil {
		io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer upstream.Close()

	// Origin form: the upstream is the real server, not a proxy, so it wants
	// the path and its own Host header back.
	req.RequestURI = ""
	req.URL.Scheme = ""
	req.URL.Host = ""
	req.Close = true
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authorization")
	if err := req.Write(upstream); err != nil {
		io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	_ = client.SetReadDeadline(time.Time{})

	// http.ReadResponse knows where the body ends (Content-Length, chunked,
	// 206, HEAD), and Response.Write streams it back, so a large media body is
	// copied rather than buffered.
	br := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer resp.Body.Close()
	_ = resp.Write(client)
}
