package netproxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// ssuri.go: the Shadowsocks forms a user actually pastes.
//
// Three of them are in circulation and all three turn up in the same box:
//
//	ss://BASE64(method:password)@host:port      SIP002, base64 user info
//	ss://method:password@host:port              SIP002, plain user info
//	ss://BASE64(method:password@host:port)      SIP002, everything encoded
//
// and a client's "copy config" gives a JSON object with server / server_port /
// method / password instead. The base64 is URL-safe, may be unpadded, and may
// be split from the host by a fragment that names the node — all of which is
// normalised here rather than pushed onto the user.

// parseSSURI reads any of the ss:// forms.
func parseSSURI(raw string) (Spec, error) {
	body := raw[len("ss://"):]
	// Drop the fragment (the node's display name) and any query (plugin
	// options; a plugin cannot run here, and silently ignoring it would be
	// worse than saying so).
	body, pluginQuery, _ := strings.Cut(body, "#")
	if q := strings.IndexByte(body, '?'); q >= 0 {
		pluginQuery = body[q+1:]
		body = body[:q]
	}
	if pluginQuery != "" && strings.Contains(strings.ToLower(pluginQuery), "plugin=") {
		return Spec{}, fmt.Errorf("这条 Shadowsocks 链接带了插件(plugin),这里不支持;请在服务器端关掉插件后再用")
	}
	if body == "" {
		return Spec{}, fmt.Errorf("Shadowsocks 链接为空: %q", raw)
	}

	// Form 1 and 2: user info before the last '@'.
	if at := strings.LastIndex(body, "@"); at >= 0 {
		userInfo, hostPort := body[:at], body[at+1:]
		method, password, err := splitSSUserInfo(userInfo)
		if err != nil {
			return Spec{}, err
		}
		hostPort = strings.TrimRight(hostPort, "/")
		if _, _, err := net.SplitHostPort(hostPort); err != nil {
			// A missing port is not something to guess: Shadowsocks servers are
			// not on a conventional port.
			return Spec{}, fmt.Errorf("Shadowsocks 地址缺少端口: %q(应形如 ss://method:password@host:8388)", raw)
		}
		return Spec{
			Scheme: SchemeSS, Host: hostPort,
			Method: method, Password: password, Raw: raw,
		}, nil
	}

	// Form 3: everything encoded.
	decoded, err := decodeBase64Loose(body)
	if err != nil {
		return Spec{}, fmt.Errorf("Shadowsocks 链接无法解析(不是合法的 base64): %q", raw)
	}
	at := strings.LastIndex(decoded, "@")
	if at < 0 {
		return Spec{}, fmt.Errorf("Shadowsocks 链接缺少主机: %q", raw)
	}
	method, password, err := splitSSUserInfo(decoded[:at])
	if err != nil {
		return Spec{}, err
	}
	hostPort := strings.TrimRight(decoded[at+1:], "/")
	if _, _, err := net.SplitHostPort(hostPort); err != nil {
		return Spec{}, fmt.Errorf("Shadowsocks 地址缺少端口: %q", decoded)
	}
	return Spec{Scheme: SchemeSS, Host: hostPort, Method: method, Password: password, Raw: raw}, nil
}

// splitSSUserInfo accepts both "method:password" and its base64 encoding. A
// plain form is percent-decoded, because a share link encodes the characters a
// URL cannot carry (@, :, /) and a password is exactly where those turn up.
func splitSSUserInfo(s string) (method, password string, err error) {
	if !strings.Contains(s, ":") {
		decoded, derr := decodeBase64Loose(s)
		if derr != nil {
			return "", "", fmt.Errorf("Shadowsocks 用户信息无法解析: %q(应形如 method:password)", s)
		}
		s = decoded
	} else if unescaped, uerr := url.PathUnescape(s); uerr == nil {
		s = unescaped
	}
	method, password, ok := strings.Cut(s, ":")
	if !ok || method == "" {
		return "", "", fmt.Errorf("Shadowsocks 用户信息无法解析: %q(应形如 method:password)", s)
	}
	return strings.TrimSpace(method), password, nil
}

// decodeBase64Loose decodes URL-safe or standard base64 with or without
// padding, which is what clients write.
func decodeBase64Loose(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("empty")
	}
	trimmed := strings.TrimRight(s, "=")
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding,
	} {
		if b, err := enc.DecodeString(trimmed); err == nil {
			return string(b), nil
		}
	}
	return "", fmt.Errorf("invalid base64")
}

// parseSSJSON reads a client's "copy config" object. Only the fields a plain
// AEAD server needs are used; a plugin is refused rather than ignored.
func parseSSJSON(text string) (Spec, error) {
	var cfg struct {
		Server     string `json:"server"`
		ServerPort any    `json:"server_port"`
		Method     string `json:"method"`
		Password   string `json:"password"`
		Plugin     string `json:"plugin"`
	}
	if err := json.Unmarshal([]byte(text), &cfg); err != nil {
		return Spec{}, fmt.Errorf("Shadowsocks 配置无法解析: %w", err)
	}
	if cfg.Plugin != "" {
		return Spec{}, fmt.Errorf("这份 Shadowsocks 配置用了插件(%s),这里不支持;请在服务器端关掉插件后再用", cfg.Plugin)
	}
	if strings.TrimSpace(cfg.Server) == "" {
		return Spec{}, fmt.Errorf("Shadowsocks 配置里没有 server")
	}
	port := ""
	switch v := cfg.ServerPort.(type) {
	case float64:
		port = strconv.Itoa(int(v))
	case string:
		port = strings.TrimSpace(v)
	}
	if port == "" {
		return Spec{}, fmt.Errorf("Shadowsocks 配置里没有 server_port")
	}
	return Spec{
		Scheme:   SchemeSS,
		Host:     net.JoinHostPort(strings.TrimSpace(cfg.Server), port),
		Method:   strings.TrimSpace(cfg.Method),
		Password: cfg.Password,
		Raw:      text,
	}, nil
}

// ssURI renders a spec back into a share link, so a config can be shown as
// something the user recognises. The password is included; callers only use
// this for the administrator's own form.
func (s Spec) ssURI() string {
	if s.Scheme != SchemeSS {
		return s.Raw
	}
	userInfo := base64.RawURLEncoding.EncodeToString([]byte(s.Method + ":" + s.Password))
	return "ss://" + userInfo + "@" + s.Host
}

// redactURLPassword masks the password in a URL's user info.
func redactURLPassword(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, has := u.User.Password(); has {
		u.User = url.UserPassword(u.User.Username(), "******")
	} else {
		u.User = url.User("******")
	}
	return u.String()
}
