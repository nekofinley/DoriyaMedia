package source

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// ErrUnsafeStreamHeaders marks a header set that may not be sent to a native
// client. Callers should treat the whole direct-http candidate as ineligible;
// they must not try to salvage individual headers from it.
var ErrUnsafeStreamHeaders = errors.New("unsafe stream headers")

// StreamHeaderError describes a rejected header without retaining its value.
// Header values can contain source credentials, so validation errors intentionally
// contain only the canonical field name and a stable reason.
type StreamHeaderError struct {
	Name   string
	Reason string
}

func (e *StreamHeaderError) Error() string {
	if e == nil {
		return ErrUnsafeStreamHeaders.Error()
	}
	if e.Name == "" {
		return "stream headers: " + e.Reason
	}
	return fmt.Sprintf("stream header %q: %s", e.Name, e.Reason)
}

func (e *StreamHeaderError) Unwrap() error {
	return ErrUnsafeStreamHeaders
}

// StreamHeaders is the complete DTO that may be sent to a native client for a
// direct source request. Its fields are fixed on purpose: arbitrary maps can
// carry Authorization, Cookie, WebDAV Basic, OpenList, or other source secrets.
type StreamHeaders struct {
	Referer        string `json:"Referer,omitempty"`
	Origin         string `json:"Origin,omitempty"`
	UserAgent      string `json:"User-Agent,omitempty"`
	Accept         string `json:"Accept,omitempty"`
	AcceptLanguage string `json:"Accept-Language,omitempty"`
}

const (
	maxRefererLength        = 2048
	maxOriginLength         = 512
	maxUserAgentLength      = 512
	maxAcceptLength         = 512
	maxAcceptLanguageLength = 256
	maxStreamHeaderCount    = 5
)

// NewStreamHeaders converts a source driver's header map into the fixed,
// client-safe DTO. The whole set is rejected if any header is unknown, repeated
// case-insensitively, oversized, or malformed.
func NewStreamHeaders(headers map[string]string) (StreamHeaders, error) {
	var out StreamHeaders
	if len(headers) > maxStreamHeaderCount {
		return out, &StreamHeaderError{Reason: "too many headers"}
	}

	keys := make([]string, 0, len(headers))
	for name := range headers {
		keys = append(keys, name)
	}
	sort.Strings(keys)

	seen := make(map[string]bool, len(headers))
	for _, rawName := range keys {
		name, ok := canonicalStreamHeaderName(rawName)
		if !ok {
			return StreamHeaders{}, &StreamHeaderError{Name: rawName, Reason: "header is not allowed"}
		}
		if seen[name] {
			return StreamHeaders{}, &StreamHeaderError{Name: name, Reason: "duplicate header name"}
		}
		seen[name] = true

		value := headers[rawName]
		if value == "" {
			return StreamHeaders{}, &StreamHeaderError{Name: name, Reason: "value is empty"}
		}
		switch name {
		case "Referer":
			out.Referer = value
		case "Origin":
			out.Origin = value
		case "User-Agent":
			out.UserAgent = value
		case "Accept":
			out.Accept = value
		case "Accept-Language":
			out.AcceptLanguage = value
		}
	}

	if err := out.Validate(); err != nil {
		return StreamHeaders{}, err
	}
	return out, nil
}

// NewStreamHeadersForURL constructs the DTO with the additional constraints
// needed before a native client receives source headers. NewStreamHeaders only
// validates the shape of individual values; Origin must additionally match the
// exact destination origin, and User-Agent/Accept must belong to reviewed
// profiles. If this stricter check fails, direct-http is ineligible and the
// server must use a proxy/HLS fallback.
func NewStreamHeadersForURL(directURL string, headers map[string]string) (StreamHeaders, error) {
	out, err := NewStreamHeaders(headers)
	if err != nil {
		return StreamHeaders{}, err
	}
	target, err := parseDirectHeaderURL(directURL)
	if err != nil {
		return StreamHeaders{}, err
	}
	if out.Empty() {
		return out, nil
	}
	if out.Origin != "" && !sameURLOrigin(target, out.Origin) {
		return StreamHeaders{}, &StreamHeaderError{
			Name: "Origin", Reason: "must exactly match the direct media origin",
		}
	}
	if out.UserAgent != "" && !directUserAgentAllowed(out.UserAgent) {
		return StreamHeaders{}, &StreamHeaderError{
			Name: "User-Agent", Reason: "must use a reviewed native-client profile",
		}
	}
	if out.Accept != "" && !directAcceptAllowed(out.Accept) {
		return StreamHeaders{}, &StreamHeaderError{
			Name: "Accept", Reason: "must contain only reviewed media MIME types",
		}
	}
	if out.AcceptLanguage != "" && !directAcceptLanguageAllowed(out.AcceptLanguage) {
		return StreamHeaders{}, &StreamHeaderError{
			Name: "Accept-Language", Reason: "must be a bounded language list",
		}
	}
	return out, nil
}

// Validate checks a manually constructed StreamHeaders value. NewStreamHeaders
// calls it as well, so callers should normally use the constructor.
func (h StreamHeaders) Validate() error {
	fields := []struct {
		name string
		val  string
		max  int
	}{
		{"Referer", h.Referer, maxRefererLength},
		{"Origin", h.Origin, maxOriginLength},
		{"User-Agent", h.UserAgent, maxUserAgentLength},
		{"Accept", h.Accept, maxAcceptLength},
		{"Accept-Language", h.AcceptLanguage, maxAcceptLanguageLength},
	}
	for _, field := range fields {
		if field.val == "" {
			continue
		}
		if err := validateStreamHeaderValue(field.name, field.val, field.max); err != nil {
			return err
		}
	}
	if h.Referer != "" && !validReferer(h.Referer) {
		return &StreamHeaderError{Name: "Referer", Reason: "must be an approved provider referer without query, userinfo, or fragment"}
	}
	if h.Origin != "" && !validOrigin(h.Origin) {
		return &StreamHeaderError{Name: "Origin", Reason: "must be an HTTP(S) origin without userinfo, path, query, or fragment"}
	}
	return nil
}

// Len reports how many headers the DTO will serialize.
func (h StreamHeaders) Len() int {
	n := 0
	for _, value := range []string{h.Referer, h.Origin, h.UserAgent, h.Accept, h.AcceptLanguage} {
		if value != "" {
			n++
		}
	}
	return n
}

// Empty reports whether the DTO contains no headers.
func (h StreamHeaders) Empty() bool {
	return h.Len() == 0
}

func canonicalStreamHeaderName(name string) (string, bool) {
	switch {
	case strings.EqualFold(name, "Referer"):
		return "Referer", true
	case strings.EqualFold(name, "Origin"):
		return "Origin", true
	case strings.EqualFold(name, "User-Agent"):
		return "User-Agent", true
	case strings.EqualFold(name, "Accept"):
		return "Accept", true
	case strings.EqualFold(name, "Accept-Language"):
		return "Accept-Language", true
	default:
		return "", false
	}
}

func validateStreamHeaderValue(name, value string, maxLen int) error {
	if value == "" {
		return &StreamHeaderError{Name: name, Reason: "value is empty"}
	}
	if len(value) > maxLen {
		return &StreamHeaderError{Name: name, Reason: "value is too long"}
	}
	if strings.TrimSpace(value) != value {
		return &StreamHeaderError{Name: name, Reason: "value has leading or trailing whitespace"}
	}
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b == '\r' || b == '\n' || b == 0 || (b < 0x20 && b != '\t') || b == 0x7f {
			return &StreamHeaderError{Name: name, Reason: "value contains forbidden control characters"}
		}
	}
	return nil
}

func validReferer(value string) bool {
	if !fixedProviderReferer(value) {
		return false
	}
	if strings.ContainsAny(value, " \t\r\n\x00#\\") {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.Opaque != "" || u.User != nil || u.Fragment != "" ||
		u.RawQuery != "" || u.ForceQuery || !validURLAuthority(u) {
		return false
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	return u.Hostname() != ""
}

// fixedProviderReferer keeps client-facing direct playback headers bounded to
// referers emitted by reviewed providers. Arbitrary source headers still work
// through the server-side proxy, but never become native-client headers.
func fixedProviderReferer(value string) bool {
	switch value {
	case biliReferer:
		return true
	default:
		return false
	}
}

func validOrigin(value string) bool {
	if strings.ContainsAny(value, " \t\r\n\x00?#\\") {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.Opaque != "" || u.User != nil || !validURLAuthority(u) {
		return false
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	if u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	return u.Hostname() != ""
}

func parseDirectHeaderURL(value string) (*url.URL, error) {
	if strings.ContainsAny(value, " \t\r\n\x00#\\") {
		return nil, &StreamHeaderError{Reason: "direct URL is invalid"}
	}
	u, err := url.Parse(value)
	if err != nil || u.Opaque != "" || u.User != nil || u.Fragment != "" ||
		!validURLAuthority(u) {
		return nil, &StreamHeaderError{Reason: "direct URL is invalid"}
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return nil, &StreamHeaderError{Reason: "direct URL scheme is not allowed"}
	}
	return u, nil
}

func sameURLOrigin(target *url.URL, rawOrigin string) bool {
	origin, err := url.Parse(rawOrigin)
	if err != nil || !validOrigin(rawOrigin) {
		return false
	}
	return strings.EqualFold(target.Scheme, origin.Scheme) &&
		strings.EqualFold(target.Hostname(), origin.Hostname()) &&
		effectiveURLPort(target) == effectiveURLPort(origin)
}

func effectiveURLPort(u *url.URL) string {
	if u == nil {
		return ""
	}
	if port := u.Port(); port != "" {
		return port
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	if strings.EqualFold(u.Scheme, "http") {
		return "80"
	}
	return ""
}

func directUserAgentAllowed(value string) bool {
	return value == biliUA
}

func directAcceptAllowed(value string) bool {
	allowed := map[string]bool{
		"*/*":                           true,
		"application/octet-stream":      true,
		"audio/*":                       true,
		"video/*":                       true,
		"application/vnd.apple.mpegurl": true,
		"application/x-mpegurl":         true,
		"application/dash+xml":          true,
		"video/mp4":                     true,
		"video/webm":                    true,
		"video/x-matroska":              true,
		"audio/mp4":                     true,
		"audio/mpeg":                    true,
	}
	for _, part := range strings.Split(value, ",") {
		if !allowed[strings.ToLower(strings.TrimSpace(part))] {
			return false
		}
	}
	return true
}

func directAcceptLanguageAllowed(value string) bool {
	for i := 0; i < len(value); i++ {
		b := value[i]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') || b == '-' || b == ',' || b == ' ' ||
			b == ';' || b == '=' || b == '.' || b == '_' || b == '*' {
			continue
		}
		return false
	}
	return true
}

func validURLAuthority(u *url.URL) bool {
	if u == nil || u.Host == "" || u.Hostname() == "" || strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return true
}
