package media

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

// egress.go: the only network path ffmpeg and ffprobe are allowed to use for
// remote media.
//
// The tools have their own HTTP stacks, so checking a URL in Go before starting
// them is not enough: a redirect, HLS child playlist or DASH segment is fetched
// by the child itself. Instead every remote input gets a short-lived
// loopback-only HTTP proxy. The child still sees the original URL, but every
// connection, redirect and nested request passes through this policy.
//
// Private destinations are not a blanket exception for a source. They are
// allowed only when the origin exactly matches an administrator-configured
// WebDAV/OpenList base, the configured driver issued a short-lived lease for
// that exact target URL, and the target is this proxy's initial origin. That
// keeps LAN libraries working without turning an arbitrary .strm URL into an
// SSRF primitive. DNS is resolved for each new connection and the dial uses the
// validated IP, never the hostname, so a second resolution cannot rebind it.

type egressIPLookup func(context.Context, string) ([]net.IPAddr, error)
type egressDialFunc func(context.Context, string, string) (net.Conn, error)

const egressConnectHandshakeLimit = 15 * time.Second

type egressTrust struct {
	mu         sync.RWMutex
	origins    map[string]struct{}
	configured map[string]struct{}
	leases     map[string]egressTargetLease
	scoped     bool
}

type egressTrustContextKey struct{}

type egressTargetLease struct {
	origin  string
	expires time.Time
}

func newEgressTrust() *egressTrust {
	return &egressTrust{
		origins:    map[string]struct{}{},
		configured: map[string]struct{}{},
		leases:     map[string]egressTargetLease{},
	}
}

func (t *egressTrust) addURL(raw string) {
	if t == nil {
		return
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil {
		return
	}
	origin, err := egressOrigin(u)
	if err != nil {
		return
	}
	t.mu.Lock()
	t.origins[origin] = struct{}{}
	t.configured[origin] = struct{}{}
	t.mu.Unlock()
}

func (t *egressTrust) addSource(src *models.Source) {
	if t == nil || src == nil || src.Config == "" {
		return
	}
	var cfg models.SourceConfig
	if json.Unmarshal([]byte(src.Config), &cfg) != nil {
		return
	}
	switch src.Type {
	case "webdav":
		t.addURL(cfg.WebDAVURL)
	case "openlist":
		raw := strings.TrimSpace(cfg.BaseURL)
		lower := strings.ToLower(raw)
		if raw != "" && !strings.HasPrefix(lower, "http://") &&
			!strings.HasPrefix(lower, "https://") {
			raw = "http://" + raw
		}
		t.addURL(raw)
	}
}

func (t *egressTrust) addDriver(drv source.Driver) {
	switch d := drv.(type) {
	case *source.WebDAVDriver:
		t.addURL(d.Base)
	case *source.OpenListDriver:
		t.addURL(d.Base)
	}
}

func egressTrustForDriver(drv source.Driver) *egressTrust {
	trust := newEgressTrust()
	trust.scoped = true
	trust.addDriver(drv)
	return trust
}

func (t *egressTrust) allows(u *url.URL) bool {
	if t == nil {
		return false
	}
	origin, err := egressOrigin(u)
	if err != nil {
		return false
	}
	t.mu.RLock()
	_, ok := t.origins[origin]
	t.mu.RUnlock()
	return ok
}

// leaseDriverTarget records a short-lived, exact-target authorization after a
// configured WebDAV/OpenList driver resolves one of its own URLs. The process
// wide trust set identifies configured origins; it does not by itself permit
// every other file or .strm URL that happens to use one of those origins.
func (t *egressTrust) leaseDriverTarget(drv source.Driver, raw string) bool {
	if t == nil {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil {
		return false
	}
	origin, err := egressOrigin(u)
	if err != nil || !egressTrustForDriver(drv).allows(u) {
		return false
	}
	key := egressTargetKey(u)
	t.mu.Lock()
	t.configured[origin] = struct{}{}
	if len(t.leases) > 512 {
		now := time.Now()
		for leaseKey, lease := range t.leases {
			if now.After(lease.expires) {
				delete(t.leases, leaseKey)
			}
		}
	}
	t.leases[key] = egressTargetLease{
		origin:  origin,
		expires: time.Now().Add(resolveTTL + time.Minute),
	}
	t.mu.Unlock()
	return true
}

// targetView narrows a process-wide trust set to one resolved target. A scoped
// trust (created from a concrete driver or this method) is already safe to use
// as-is; an unscoped process-wide set grants nothing without an active lease.
func (t *egressTrust) targetView(raw string) *egressTrust {
	if t == nil {
		return newEgressTrust()
	}
	t.mu.RLock()
	if t.scoped {
		t.mu.RUnlock()
		return t
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		t.mu.RUnlock()
		return newEgressTrust()
	}
	lease, ok := t.leases[egressTargetKey(u)]
	if !ok || time.Now().After(lease.expires) {
		t.mu.RUnlock()
		return newEgressTrust()
	}
	t.mu.RUnlock()

	view := newEgressTrust()
	view.scoped = true
	view.origins[lease.origin] = struct{}{}
	view.configured[lease.origin] = struct{}{}
	return view
}

func egressTargetKey(u *url.URL) string {
	if u == nil {
		return ""
	}
	origin, err := egressOrigin(u)
	if err != nil {
		return ""
	}
	return origin + u.EscapedPath() + "?" + u.RawQuery
}

func withEgressTrust(ctx context.Context, trust *egressTrust) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, egressTrustContextKey{}, trust)
}

func egressTrustFromContext(ctx context.Context, fallback *egressTrust) *egressTrust {
	if ctx != nil {
		if trust, ok := ctx.Value(egressTrustContextKey{}).(*egressTrust); ok {
			return trust
		}
	}
	return fallback
}

type egressTarget struct {
	URL   string
	Proxy string
}

type egressOptions struct {
	trust       *egressTrust
	lookup      egressIPLookup
	dial        egressDialFunc
	denyAll     bool
	denyConnect bool
}

type egressSchemeContextKey struct{}

type egressController struct {
	ln        net.Listener
	server    *http.Server
	policy    *egressPolicy
	transport *http.Transport
	closeMu   sync.Once
}

func newEgressController(targets []egressTarget, opts egressOptions) (*egressController, error) {
	initial := make(map[string]struct{}, len(targets))
	initialScheme := ""
	var sourceProxy *url.URL
	for _, target := range targets {
		u, err := url.Parse(target.URL)
		if err != nil || u.User != nil {
			return nil, errors.New("media egress: invalid remote target")
		}
		origin, err := egressOrigin(u)
		if err != nil {
			return nil, fmt.Errorf("media egress: %w", err)
		}
		scheme := strings.ToLower(u.Scheme)
		if initialScheme != "" && initialScheme != scheme {
			// A forward proxy has no request state that ties this request back
			// to one initial URL. Refuse a mixed-scheme controller rather than
			// letting an HTTPS input's HTTP child borrow an HTTP input's policy.
			return nil, errors.New("media egress: mixed initial URL schemes")
		}
		initialScheme = scheme
		initial[origin] = struct{}{}

		if strings.TrimSpace(target.Proxy) == "" {
			if sourceProxy != nil {
				return nil, errors.New("media egress: inconsistent source proxies")
			}
			continue
		}
		proxyURL, err := url.Parse(target.Proxy)
		if err != nil || proxyURL.Scheme != "http" || proxyURL.Hostname() == "" {
			return nil, errors.New("media egress: invalid source proxy")
		}
		if !egressLoopbackHost(proxyURL.Hostname()) {
			return nil, errors.New("media egress: source proxy must be a loopback bridge")
		}
		if sourceProxy != nil && sourceProxy.String() != proxyURL.String() {
			return nil, errors.New("media egress: inconsistent source proxies")
		}
		sourceProxy = proxyURL
	}
	if len(initial) == 0 && !opts.denyAll {
		return nil, errors.New("media egress: no remote targets")
	}

	lookup := opts.lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	dial := opts.dial
	if dial == nil {
		d := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
		dial = d.DialContext
	}
	policy := &egressPolicy{
		trust:          opts.trust,
		initialOrigins: initial,
		lookup:         lookup,
		dial:           dial,
		sourceProxy:    sourceProxy,
		denyAll:        opts.denyAll,
		denyConnect:    opts.denyConnect,
		originScope:    map[string]egressAddressScope{},
		allowHTTP:      initialScheme == "http",
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           policy.dialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("media egress: listen: %w", err)
	}
	c := &egressController{ln: ln, policy: policy, transport: transport}
	c.server = &http.Server{
		Handler:           http.HandlerFunc(c.serveHTTP),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	go func() {
		_ = c.server.Serve(ln)
	}()
	return c, nil
}

func (c *egressController) ProxyURL() string {
	if c == nil || c.ln == nil {
		return ""
	}
	return "http://" + c.ln.Addr().String()
}

func (c *egressController) Close() {
	if c == nil {
		return
	}
	c.closeMu.Do(func() {
		_ = c.server.Close()
		_ = c.ln.Close()
		c.transport.CloseIdleConnections()
	})
}

func (c *egressController) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if c == nil || c.policy == nil {
		http.Error(w, "media egress unavailable", http.StatusBadGateway)
		return
	}
	if r.Method == http.MethodConnect {
		c.serveConnect(w, r)
		return
	}
	if r.URL == nil || !r.URL.IsAbs() {
		w.Header().Set("Connection", "close")
		http.Error(w, "absolute proxy request required", http.StatusMethodNotAllowed)
		return
	}
	c.serveForward(w, r)
}

func (c *egressController) serveConnect(w http.ResponseWriter, r *http.Request) {
	if c.policy.denyConnect {
		http.Error(w, "media egress rejected encrypted tunnel", http.StatusForbidden)
		return
	}
	target := r.Host
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		host = strings.Trim(target, "[]")
		port = "443"
	}
	if port == "" {
		port = "443"
	}
	u := &url.URL{Scheme: "https", Host: net.JoinHostPort(host, port)}
	conn, err := c.policy.connect(r.Context(), u)
	if err != nil {
		http.Error(w, "media egress rejected destination", http.StatusForbidden)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = conn.Close()
		http.Error(w, "media egress unavailable", http.StatusBadGateway)
		return
	}
	client, rw, err := hijacker.Hijack()
	if err != nil {
		_ = conn.Close()
		return
	}
	_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(conn, rw)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, conn)
		if cw, ok := client.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	_ = conn.Close()
	_ = client.Close()
}

func (c *egressController) serveForward(w http.ResponseWriter, r *http.Request) {
	if r.URL.Scheme != "http" && r.URL.Scheme != "https" {
		http.Error(w, "media egress rejected scheme", http.StatusForbidden)
		return
	}
	if r.URL.Scheme == "http" && !c.policy.allowHTTP {
		http.Error(w, "media egress rejected plaintext downgrade", http.StatusForbidden)
		return
	}
	if _, _, err := c.policy.authorize(r.Context(), r.URL); err != nil {
		http.Error(w, "media egress rejected destination", http.StatusForbidden)
		return
	}

	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Host = out.URL.Host
	out.Header = r.Header.Clone()
	for _, name := range []string{
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Proxy-Connection", "TE", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		out.Header.Del(name)
	}
	origin, _ := egressOrigin(out.URL)
	if !c.policy.isInitialOrigin(origin) {
		// A redirect or nested HLS/DASH object on another origin must never
		// receive source credentials. Range and the normal media headers stay.
		egressStripCrossOriginHeaders(out.Header)
	}
	out.Header.Set("Accept-Encoding", "identity")
	// net/http passes only host:port to DialContext. Carry the scheme from the
	// request so an HTTPS trust entry can never be reused for HTTP (or the
	// reverse) on the same host and port.
	out = out.WithContext(context.WithValue(
		out.Context(), egressSchemeContextKey{}, strings.ToLower(out.URL.Scheme)))

	resp, err := c.transport.RoundTrip(out)
	if err != nil {
		http.Error(w, "media egress upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if location := resp.Header.Get("Location"); location != "" {
		redirect, err := out.URL.Parse(location)
		if err != nil || redirect.User != nil ||
			(strings.ToLower(redirect.Scheme) != "http" &&
				strings.ToLower(redirect.Scheme) != "https") {
			http.Error(w, "media egress rejected redirect", http.StatusBadGateway)
			return
		}
		if strings.EqualFold(out.URL.Scheme, "https") &&
			strings.EqualFold(redirect.Scheme, "http") {
			http.Error(w, "media egress rejected plaintext redirect", http.StatusForbidden)
			return
		}
	}

	for _, name := range []string{
		"Accept-Ranges", "Cache-Control", "Content-Encoding", "Content-Length",
		"Content-Range", "Content-Type", "ETag", "Expires", "Last-Modified", "Location",
	} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, resp.Body)
}

func egressStripCrossOriginHeaders(h http.Header) {
	keep := map[string]bool{
		"Accept":          true,
		"Accept-Encoding": true,
		"If-Range":        true,
		"Range":           true,
		"User-Agent":      true,
	}
	for name := range h {
		if !keep[http.CanonicalHeaderKey(name)] {
			h.Del(name)
		}
	}
}

type egressPolicy struct {
	trust          *egressTrust
	initialOrigins map[string]struct{}
	lookup         egressIPLookup
	dial           egressDialFunc
	sourceProxy    *url.URL
	denyAll        bool
	// denyConnect is set for plaintext inputs whose headers are not safe to
	// expose through an opaque TLS tunnel. Same-origin HTTP remains supported.
	denyConnect bool
	// A controller belongs to one media input. Once that input starts with
	// HTTPS, plaintext HTTP cannot be part of its redirect or nested resource
	// graph. This startup decision needs no request history.
	allowHTTP bool

	classMu     sync.Mutex
	originScope map[string]egressAddressScope
}

func (p *egressPolicy) isInitialOrigin(origin string) bool {
	_, ok := p.initialOrigins[origin]
	return ok
}

func (p *egressPolicy) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	scheme, _ := ctx.Value(egressSchemeContextKey{}).(string)
	if scheme != "http" && scheme != "https" {
		return nil, errors.New("media egress: missing destination scheme")
	}
	u := &url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port)}
	return p.connect(ctx, u)
}

func (p *egressPolicy) connect(ctx context.Context, u *url.URL) (net.Conn, error) {
	ips, _, err := p.authorize(ctx, u)
	if err != nil {
		return nil, err
	}
	port := egressEffectivePort(u)
	var lastErr error
	for _, ip := range ips {
		target := net.JoinHostPort(ip.String(), port)
		var conn net.Conn
		if p.sourceProxy != nil {
			conn, err = dialHTTPProxyBridge(ctx, p.sourceProxy, target, p.dial)
		} else {
			conn, err = p.dial(ctx, "tcp", target)
		}
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("media egress: no destination address")
	}
	return nil, lastErr
}

func (p *egressPolicy) authorize(
	ctx context.Context,
	u *url.URL,
) ([]net.IP, string, error) {
	if p == nil || p.denyAll {
		return nil, "", errors.New("media egress: destination denied")
	}
	origin, err := egressOrigin(u)
	if err != nil {
		return nil, "", err
	}
	host := egressCanonicalHost(u.Hostname())
	if host == "" {
		return nil, "", errors.New("media egress: empty host")
	}
	ips, err := p.resolve(ctx, host)
	if err != nil {
		return nil, "", err
	}

	var private, public, forbidden, loopback, networkPrivate bool
	for _, ip := range ips {
		switch egressIPClass(ip) {
		case egressIPForbidden:
			forbidden = true
		case egressIPPrivate:
			private = true
			if ip.IsLoopback() {
				loopback = true
			} else {
				networkPrivate = true
			}
		default:
			public = true
		}
	}
	if forbidden {
		return nil, "", errors.New("media egress: forbidden destination address")
	}
	if private && public {
		return nil, "", errors.New("media egress: mixed public and private destination addresses")
	}
	if loopback && networkPrivate {
		return nil, "", errors.New("media egress: mixed loopback and private destination addresses")
	}
	if private {
		// Private access needs both: an explicit admin trust entry, and the
		// destination must be one of this invocation's initial origins. Neither
		// a redirect nor an HLS child can widen that boundary.
		if !p.isInitialOrigin(origin) || !p.trust.allows(u) {
			return nil, "", errors.New("media egress: private destination rejected")
		}
	}
	scope := egressAddressPublic
	if loopback {
		scope = egressAddressLoopback
	} else if private {
		scope = egressAddressPrivate
	}
	if err := p.pinOriginClass(origin, scope); err != nil {
		return nil, "", err
	}
	return ips, origin, nil
}

type egressAddressScope int

const (
	egressAddressPublic egressAddressScope = iota
	egressAddressPrivate
	egressAddressLoopback
)

func (p *egressPolicy) pinOriginClass(origin string, scope egressAddressScope) error {
	p.classMu.Lock()
	defer p.classMu.Unlock()
	if expected, ok := p.originScope[origin]; ok {
		if expected != scope {
			return errors.New("media egress: destination address class changed")
		}
		return nil
	}
	p.originScope[origin] = scope
	return nil
}

func (p *egressPolicy) resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return []net.IP{ip}, nil
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, nil
	}
	addrs, err := p.lookup(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, errors.New("media egress: destination resolution failed")
	}
	out := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if addr.IP == nil {
			return nil, errors.New("media egress: destination resolution failed")
		}
		out = append(out, addr.IP)
	}
	return out, nil
}

type egressIPClassValue int

const (
	egressIPPublic egressIPClassValue = iota
	egressIPPrivate
	egressIPForbidden
)

func egressIPClass(ip net.IP) egressIPClassValue {
	if egressKnownMetadataIP(ip) {
		return egressIPForbidden
	}
	if ip == nil {
		return egressIPForbidden
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return egressIPPrivate
	}
	if embedded := egressTransitionIPv4(ip); embedded != nil {
		if egressIPClass(embedded) == egressIPPublic {
			return egressIPPublic
		}
		// NAT64 and 6to4 can encode an otherwise private IPv4 destination.
		// Do not let an IPv6 literal turn that into a trusted LAN origin.
		return egressIPForbidden
	}
	if ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || !ip.IsGlobalUnicast() {
		return egressIPForbidden
	}
	if v4 := ip.To4(); v4 != nil {
		// Shared address space and benchmark networks are not public media
		// origins and have historically been used for local-service pivots.
		if v4[0] == 0 ||
			(v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127) ||
			(v4[0] == 198 && (v4[1] == 18 || v4[1] == 19)) ||
			(v4[0] == 192 && v4[1] == 0 && v4[2] == 0) {
			return egressIPForbidden
		}
	}
	return egressIPPublic
}

func egressKnownMetadataIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		// Alibaba Cloud and the Azure platform endpoint are public-looking
		// numeric ranges but must never become media destinations.
		return v4.Equal(net.IPv4(100, 100, 100, 200)) ||
			v4.Equal(net.IPv4(168, 63, 129, 16))
	}
	// AWS IMDS over IPv6.
	return ip.Equal(net.ParseIP("fd00:ec2::254"))
}

func egressTransitionIPv4(ip net.IP) net.IP {
	if ip == nil || ip.To4() != nil || len(ip) != net.IPv6len {
		return nil
	}
	// Deprecated IPv4-compatible ::/96 addresses.
	allZero := true
	for _, value := range ip[:12] {
		if value != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return net.IPv4(ip[12], ip[13], ip[14], ip[15])
	}
	// Well-known NAT64 prefix 64:ff9b::/96.
	if ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff &&
		ip[3] == 0x9b && ip[4] == 0 && ip[5] == 0 &&
		ip[6] == 0 && ip[7] == 0 && ip[8] == 0 && ip[9] == 0 &&
		ip[10] == 0 && ip[11] == 0 {
		return net.IPv4(ip[12], ip[13], ip[14], ip[15])
	}
	// Local-use NAT64 prefix 64:ff9b:1::/48.
	if ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff &&
		ip[3] == 0x9b && ip[4] == 0x00 && ip[5] == 0x01 {
		return net.IPv4(ip[12], ip[13], ip[14], ip[15])
	}
	// Deprecated 6to4 prefix 2002::/16.
	if ip[0] == 0x20 && ip[1] == 0x02 {
		return net.IPv4(ip[2], ip[3], ip[4], ip[5])
	}
	return nil
}

func egressOrigin(u *url.URL) (string, error) {
	if u == nil || u.User != nil {
		return "", errors.New("invalid media destination")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("unsupported media destination scheme")
	}
	host := egressCanonicalHost(u.Hostname())
	if host == "" || strings.Contains(host, "%") {
		return "", errors.New("invalid media destination host")
	}
	port := egressEffectivePort(u)
	return scheme + "://" + net.JoinHostPort(host, port), nil
}

func egressCanonicalHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func egressEffectivePort(u *url.URL) string {
	if u == nil {
		return ""
	}
	if raw := u.Port(); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= 65535 {
			return strconv.Itoa(n)
		}
		return raw
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

func egressLoopbackHost(host string) bool {
	host = egressCanonicalHost(host)
	return host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		net.ParseIP(strings.Trim(host, "[]")) != nil &&
			net.ParseIP(strings.Trim(host, "[]")).IsLoopback()
}

func dialHTTPProxyBridge(
	ctx context.Context,
	proxyURL *url.URL,
	target string,
	dial egressDialFunc,
) (net.Conn, error) {
	conn, err := dial(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, err
	}
	finishHandshake, err := egressWatchConnectHandshake(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	handshakeFinished := false
	defer func() {
		if !handshakeFinished {
			_ = finishHandshake()
		}
	}()
	var request strings.Builder
	request.WriteString("CONNECT ")
	request.WriteString(target)
	request.WriteString(" HTTP/1.1\r\nHost: ")
	request.WriteString(target)
	request.WriteString("\r\n")
	if proxyURL.User != nil {
		user := proxyURL.User.Username()
		pass, _ := proxyURL.User.Password()
		token := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		request.WriteString("Proxy-Authorization: Basic ")
		request.WriteString(token)
		request.WriteString("\r\n")
	}
	request.WriteString("\r\n")
	if _, err := io.WriteString(conn, request.String()); err != nil {
		_ = conn.Close()
		return nil, egressConnectHandshakeError(ctx, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, egressConnectHandshakeError(ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("media egress proxy status %d", resp.StatusCode)
	}
	handshakeFinished = true
	if err := finishHandshake(); err != nil {
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, err
	}
	if br.Buffered() > 0 {
		return &egressBufferedConn{Conn: conn, reader: br}, nil
	}
	return conn, nil
}

func egressConnectHandshakeError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return err
}

func egressWatchConnectHandshake(
	ctx context.Context,
	conn net.Conn,
) (func() error, error) {
	deadline := time.Now().Add(egressConnectHandshakeLimit)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}

	watchDone := make(chan struct{})
	stopWatch := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
		close(watchDone)
	})
	return func() error {
		if !stopWatch() {
			<-watchDone
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return conn.SetDeadline(time.Time{})
	}, nil
}

type egressBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *egressBufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func egressRemoteTarget(target string) (bool, error) {
	scheme, hasScheme := egressInputScheme(target)
	if !hasScheme || len(scheme) == 1 {
		// No scheme (or a Windows drive letter) is a local filename. Do not
		// run url.Parse on it: valid local names may contain percent escapes
		// or other characters that net/url rejects.
		return false, nil
	}
	u, err := url.Parse(target)
	if err != nil {
		return false, errors.New("media egress: invalid input target")
	}
	switch strings.ToLower(scheme) {
	case "http", "https":
		if u.Hostname() == "" || u.User != nil {
			return false, errors.New("media egress: invalid remote input target")
		}
		return true, nil
	case "file":
		if u.Host != "" && !egressLoopbackHost(u.Hostname()) {
			return false, errors.New("media egress: remote file authority is unsupported")
		}
		return false, nil
	default:
		// Everything else fails closed, including concat:, crypto:, tcp: and
		// other protocols that can bypass an HTTP proxy.
		return false, errors.New("media egress: unsupported input scheme")
	}
}

// egressInputScheme finds an ffmpeg-style protocol prefix without rejecting
// ordinary local paths. A path component before ':' means the colon belongs to
// the filename (for example /srv/a:b.mkv), not to an input scheme.
func egressInputScheme(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch c {
		case ':':
			if i == 0 || !egressSchemeStart(raw[0]) {
				return "", false
			}
			for j := 1; j < i; j++ {
				if !egressSchemeChar(raw[j]) {
					return "", false
				}
			}
			return raw[:i], true
		case '/', '\\', '?', '#':
			return "", false
		}
	}
	return "", false
}

func egressSchemeStart(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func egressSchemeChar(c byte) bool {
	return egressSchemeStart(c) || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'
}

func secureEgressProxy(
	target, sourceProxy, rawHeaders string,
	trust *egressTrust,
) (*egressController, error) {
	remote, err := egressRemoteTarget(target)
	if err != nil {
		return nil, err
	}
	if !remote {
		if err := egressRejectLocalPlaylist(target); err != nil {
			return nil, err
		}
		// Every local probe gets a deny-all fallback so an unexpected nested
		// network URI cannot escape through an AVIO context that ignores the
		// input's protocol whitelist.
		return newEgressController(nil, egressOptions{denyAll: true})
	}
	denyConnect, err := egressHeaderConnectPolicy(target, rawHeaders)
	if err != nil {
		return nil, err
	}
	return newEgressController(
		[]egressTarget{{URL: target, Proxy: sourceProxy}},
		egressOptions{trust: trust, denyConnect: denyConnect},
	)
}

// tcp and httpproxy are required internally by ffmpeg's HTTP protocol; the
// latter is the CONNECT wrapper used for HTTPS through a proxy. Remote
// HLS/DASH inputs must never receive file: a remote manifest could otherwise
// make the child read local files.
const egressRemoteProtocolWhitelist = "http,https,httpproxy,tls,tcp,crypto,data,pipe"

// file is needed for ordinary local media. Keep HTTP out entirely; the deny-all
// fallback still protects any AVIO context that ignores this whitelist.
const egressLocalProtocolWhitelist = "file,crypto,data,pipe"

// Local media is never allowed to select a manifest demuxer. A local HLS or
// DASH playlist can name file: children outside the configured source root,
// which the protocol whitelist cannot distinguish from the main local file.
// Keep common container/audio demuxers available while forcing HLS, DASH and
// concat inputs to fail closed until they pass through a path-aware fetcher.
const egressLocalFormatWhitelist = "matroska,webm,mov,mp4,m4a,3gp,3g2,mj2," +
	"avi,mpegts,mpeg,flv,ogg,asf,wav,aiff,au,caf,mp3,aac,ac3,eac3,flac," +
	"ape,tta,wv,amr,dts,loas,rm,mxf,dv,m4v,nut"

const egressLocalManifestSniffBytes = 1 << 20

func egressRejectLocalPlaylist(target string) error {
	path, ok := egressLocalInputPath(target)
	if !ok || strings.TrimSpace(path) == "" {
		return nil
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".m3u", ".m3u8", ".mpd":
		return errors.New("media egress: local HLS/DASH playlists are not supported")
	}

	file, err := os.Open(path)
	if err != nil {
		// The child will report a normal open failure. This preflight exists to
		// stop readable manifests, not to duplicate file-access diagnostics.
		return nil
	}
	defer file.Close()
	head := make([]byte, egressLocalManifestSniffBytes)
	n, readErr := io.ReadFull(file, head)
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return nil
	}
	head = head[:n]
	if egressHLSMarkerAtStart(head) {
		return errors.New("media egress: local HLS playlists are not supported")
	}

	decoder := xml.NewDecoder(bytes.NewReader(head))
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if strings.EqualFold(start.Name.Local, "MPD") {
			return errors.New("media egress: local DASH manifests are not supported")
		}
		break
	}
	return nil
}

func egressHLSMarkerAtStart(head []byte) bool {
	head = bytes.TrimPrefix(head, []byte{0xef, 0xbb, 0xbf})
	head = bytes.TrimLeft(head, " \t\r\n")
	return bytes.HasPrefix(head, []byte("#EXTM3U"))
}

func egressLocalInputPath(raw string) (string, bool) {
	scheme, hasScheme := egressInputScheme(raw)
	if !hasScheme || len(scheme) == 1 {
		return raw, true
	}
	if !strings.EqualFold(scheme, "file") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	path := u.Opaque
	if path == "" {
		path, err = url.PathUnescape(u.EscapedPath())
		if err != nil {
			return "", false
		}
	}
	// file:///C:/... is a common spelling; Windows APIs need the drive letter
	// without the leading URL slash.
	if len(path) >= 3 && path[0] == '/' &&
		((path[1] >= 'A' && path[1] <= 'Z') || (path[1] >= 'a' && path[1] <= 'z')) &&
		path[2] == ':' {
		path = path[1:]
	}
	return filepath.FromSlash(path), true
}

type egressControllerSet struct {
	controllers []*egressController
	fallback    *egressController
	closeOnce   sync.Once
}

func (s *egressControllerSet) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		for _, controller := range s.controllers {
			controller.Close()
		}
		s.fallback.Close()
	})
}

func (s *egressControllerSet) DefaultProxyURL() string {
	if s == nil {
		return ""
	}
	if s.fallback != nil {
		return s.fallback.ProxyURL()
	}
	if len(s.controllers) == 1 {
		return s.controllers[0].ProxyURL()
	}
	return ""
}

// secureFFmpegArgs inserts a distinct controlled proxy for each remote -i
// input and removes any source proxy it replaces. Separate proxies prevent a
// public input's redirect or nested playlist from inheriting a private origin
// trusted only for another input. The returned controllers live for the child
// process; callers must Close them only after the child has exited.
func secureFFmpegArgs(
	args []string,
	trust *egressTrust,
) ([]string, *egressControllerSet, error) {
	type input struct {
		flagIndex   int
		target      string
		proxy       string
		denyConnect bool
	}
	var inputs []input
	localInputs := map[int]bool{}
	pendingProxy := ""
	pendingHeaders := ""
	proxyOption := -1
	skip := map[int]bool{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-http_proxy":
			if i+1 >= len(args) {
				return nil, nil, errors.New("media egress: missing proxy value")
			}
			pendingProxy = args[i+1]
			proxyOption = i
			i++
		case "-headers":
			if i+1 >= len(args) {
				return nil, nil, errors.New("media egress: missing header value")
			}
			pendingHeaders += args[i+1]
			i++
		case "-cookies", "-referer", "-user_agent", "-user-agent", "-auth_type", "-auth-type":
			return nil, nil, fmt.Errorf("media egress: unsupported input header option %s", args[i])
		case "-i":
			if i+1 >= len(args) {
				return nil, nil, errors.New("media egress: missing input target")
			}
			target := args[i+1]
			remote, err := egressRemoteTarget(target)
			if err != nil {
				return nil, nil, err
			}
			if remote {
				denyConnect, err := egressHeaderConnectPolicy(target, pendingHeaders)
				if err != nil {
					return nil, nil, err
				}
				inputs = append(inputs, input{
					flagIndex:   i,
					target:      target,
					proxy:       pendingProxy,
					denyConnect: denyConnect,
				})
				if proxyOption >= 0 {
					skip[proxyOption] = true
					skip[proxyOption+1] = true
				}
			} else {
				if err := egressRejectLocalPlaylist(target); err != nil {
					return nil, nil, err
				}
				localInputs[i] = true
				if proxyOption >= 0 {
					skip[proxyOption] = true
					skip[proxyOption+1] = true
				}
			}
			pendingProxy = ""
			pendingHeaders = ""
			proxyOption = -1
			i++
		}
	}
	if len(inputs) == 0 && len(localInputs) == 0 {
		return args, nil, nil
	}

	controllers := make([]*egressController, 0, len(inputs))
	for _, in := range inputs {
		controller, err := newEgressController(
			[]egressTarget{{URL: in.target, Proxy: in.proxy}},
			egressOptions{
				trust:       trust.targetView(in.target),
				denyConnect: in.denyConnect,
			})
		if err != nil {
			(&egressControllerSet{controllers: controllers}).Close()
			return nil, nil, err
		}
		controllers = append(controllers, controller)
	}
	group := &egressControllerSet{controllers: controllers}
	if len(controllers) > 1 || len(localInputs) > 0 {
		// Environment variables cannot identify which input a nested HLS/DASH
		// request came from. Never point them at one input's trusted proxy, and
		// never leave a local input without a proxy: if a child ignores its
		// per-input option, the fallback rejects instead of letting it borrow
		// another input's authorization or access the network.
		fallback, err := newEgressController(nil, egressOptions{denyAll: true})
		if err != nil {
			group.Close()
			return nil, nil, err
		}
		group.fallback = fallback
	}

	inputByTarget := make(map[int]string, len(inputs))
	for i, in := range inputs {
		inputByTarget[in.flagIndex] = controllers[i].ProxyURL()
	}
	out := make([]string, 0, len(args)+len(inputs)*4)
	for i := 0; i < len(args); i++ {
		if skip[i] {
			continue
		}
		if proxy, ok := inputByTarget[i]; ok {
			out = append(out,
				"-protocol_whitelist", egressRemoteProtocolWhitelist,
				"-http_proxy", proxy)
		} else if localInputs[i] {
			out = append(out,
				"-protocol_whitelist", egressLocalProtocolWhitelist,
				"-format_whitelist", egressLocalFormatWhitelist)
		}
		out = append(out, args[i])
	}
	return out, group, nil
}

// ffmpeg's -headers value is attached to every request made by that input's
// AVIO context. TLS hides redirect and nested HLS/DASH requests from the
// loopback proxy, so only the small, reviewed header DTO may cross an HTTPS
// input boundary. A plaintext input may carry credentials on its initial
// origin, but it must not open a TLS tunnel where those credentials would be
// invisible to the proxy. This remains fail-closed until a path-aware Go-side
// fetcher can inject and strip source credentials per request.
func egressHeaderConnectPolicy(target, rawHeaders string) (bool, error) {
	if strings.TrimSpace(rawHeaders) == "" {
		return false, nil
	}
	u, err := url.Parse(target)
	if err != nil {
		return false, errors.New("media egress: invalid input URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false, nil
	}
	headers, err := egressParseHeaderBlock(rawHeaders)
	if err != nil {
		return false, err
	}
	if _, err := source.NewStreamHeaders(headers); err != nil {
		if scheme == "https" {
			return false, errors.New("media egress: HTTPS input headers are not cross-origin safe")
		}
		return true, nil
	}
	return false, nil
}

func egressParseHeaderBlock(raw string) (map[string]string, error) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	out := make(map[string]string)
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			return nil, errors.New("media egress: folded input headers are not allowed")
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			return nil, errors.New("media egress: malformed input header")
		}
		name := strings.TrimSpace(line[:colon])
		value := strings.TrimSpace(line[colon+1:])
		if name == "" || strings.ContainsAny(name, " \t\r\n") || value == "" {
			return nil, errors.New("media egress: malformed input header")
		}
		canonical := http.CanonicalHeaderKey(name)
		if _, duplicate := out[canonical]; duplicate {
			return nil, errors.New("media egress: duplicate input header")
		}
		out[canonical] = value
	}
	return out, nil
}

func secureMediaEnv(proxyRaw string) []string {
	env := os.Environ()
	out := env[:0]
	for _, entry := range env {
		name := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			name = entry[:i]
		}
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
			"FTP_PROXY", "RSYNC_PROXY":
			continue
		}
		out = append(out, entry)
	}
	if proxyRaw = strings.TrimSpace(proxyRaw); proxyRaw != "" {
		// The explicit -http_proxy option covers the demuxer's main input.
		// These defaults also cover child AVIO contexts that do not inherit
		// per-input options, so they cannot silently fall back to direct HTTP.
		out = append(out,
			"http_proxy="+proxyRaw,
			"https_proxy="+proxyRaw)
	}
	return out
}
