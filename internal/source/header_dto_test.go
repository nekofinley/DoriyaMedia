package source

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestNewStreamHeadersAcceptsOnlyWhitelistedFields(t *testing.T) {
	raw := map[string]string{
		"referer":         "https://www.bilibili.com/",
		"ORIGIN":          "https://www.example.com",
		"user-agent":      "DoriyaMedia/0.1",
		"Accept":          "video/*",
		"accept-language": "zh-CN, en",
	}
	got, err := NewStreamHeaders(raw)
	if err != nil {
		t.Fatalf("NewStreamHeaders: %v", err)
	}
	want := StreamHeaders{
		Referer:        "https://www.bilibili.com/",
		Origin:         "https://www.example.com",
		UserAgent:      "DoriyaMedia/0.1",
		Accept:         "video/*",
		AcceptLanguage: "zh-CN, en",
	}
	if got != want {
		t.Fatalf("headers = %#v, want %#v", got, want)
	}
	if got.Len() != 5 || got.Empty() {
		t.Fatalf("Len/Empty = %d/%v, want 5/false", got.Len(), got.Empty())
	}

	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var names map[string]any
	if err := json.Unmarshal(encoded, &names); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	wantNames := []string{"Referer", "Origin", "User-Agent", "Accept", "Accept-Language"}
	for _, name := range wantNames {
		if _, ok := names[name]; !ok {
			t.Errorf("JSON header %q missing from %s", name, encoded)
		}
	}
}

func TestNewStreamHeadersEmpty(t *testing.T) {
	got, err := NewStreamHeaders(nil)
	if err != nil {
		t.Fatalf("NewStreamHeaders(nil): %v", err)
	}
	if !got.Empty() || got != (StreamHeaders{}) {
		t.Fatalf("empty headers = %#v, want zero DTO", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(encoded) != "{}" {
		t.Fatalf("empty DTO JSON = %s, want {}", encoded)
	}
}

func TestNewStreamHeadersRejectsSensitiveAndUnknownNames(t *testing.T) {
	for _, name := range []string{
		"Authorization",
		"Cookie",
		"x-openlist-token",
		"Proxy-Authorization",
		"Host",
		"Content-Type",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewStreamHeaders(map[string]string{name: "do-not-leak-this-value"})
			if !errors.Is(err, ErrUnsafeStreamHeaders) {
				t.Fatalf("error = %v, want ErrUnsafeStreamHeaders", err)
			}
			if strings.Contains(err.Error(), "do-not-leak-this-value") {
				t.Fatalf("error leaked the header value: %v", err)
			}
		})
	}
}

func TestNewStreamHeadersRejectsCaseInsensitiveDuplicates(t *testing.T) {
	_, err := NewStreamHeaders(map[string]string{
		"Referer": "https://example.com/",
		"referer": "https://other.example.com/",
	})
	if !errors.Is(err, ErrUnsafeStreamHeaders) {
		t.Fatalf("error = %v, want ErrUnsafeStreamHeaders", err)
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error = %v, want duplicate reason", err)
	}
}

func TestNewStreamHeadersRejectsTooManyHeaders(t *testing.T) {
	_, err := NewStreamHeaders(map[string]string{
		"Referer":         "https://example.com/",
		"Origin":          "https://example.com",
		"User-Agent":      "DoriyaMedia/0.1",
		"Accept":          "video/*",
		"Accept-Language": "en",
		"referer":         "https://other.example.com/",
	})
	if !errors.Is(err, ErrUnsafeStreamHeaders) {
		t.Fatalf("error = %v, want ErrUnsafeStreamHeaders", err)
	}
	if !strings.Contains(err.Error(), "too many") {
		t.Fatalf("error = %v, want too-many reason", err)
	}
}

func TestNewStreamHeadersRejectsUnsafeValues(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"CRLF", "ok\r\nX-Evil: 1"},
		{"bare CR", "ok\rnext"},
		{"bare LF", "ok\nnext"},
		{"NUL", "ok\x00next"},
		{"control", "ok\x01next"},
		{"leading space", " value"},
		{"trailing space", "value "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewStreamHeaders(map[string]string{"User-Agent": tc.value})
			if !errors.Is(err, ErrUnsafeStreamHeaders) {
				t.Fatalf("error = %v, want ErrUnsafeStreamHeaders", err)
			}
		})
	}
}

func TestNewStreamHeadersRejectsOversizedValues(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"Referer", "https://example.com/" + strings.Repeat("a", maxRefererLength)},
		{"Origin", "https://" + strings.Repeat("a", maxOriginLength) + ".example"},
		{"User-Agent", strings.Repeat("u", maxUserAgentLength+1)},
		{"Accept", strings.Repeat("a", maxAcceptLength+1)},
		{"Accept-Language", strings.Repeat("a", maxAcceptLanguageLength+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewStreamHeaders(map[string]string{tc.name: tc.value})
			if !errors.Is(err, ErrUnsafeStreamHeaders) {
				t.Fatalf("error = %v, want ErrUnsafeStreamHeaders", err)
			}
			if !strings.Contains(err.Error(), "too long") {
				t.Fatalf("error = %v, want too-long reason", err)
			}
		})
	}
}

func TestNewStreamHeadersOriginShape(t *testing.T) {
	valid := []string{
		"https://example.com",
		"http://example.com:8080",
		"https://[2001:db8::1]:443",
	}
	for _, value := range valid {
		if _, err := NewStreamHeaders(map[string]string{"Origin": value}); err != nil {
			t.Errorf("Origin %q rejected: %v", value, err)
		}
	}

	invalid := []string{
		"example.com",
		"ftp://example.com",
		"https://user@example.com",
		"https://example.com/",
		"https://example.com/path",
		"https://example.com?q=1",
		"https://example.com#fragment",
		"https://example.com:bad",
		"https://example.com:",
		"https://example.com:65536",
		"https://example.com\\path",
	}
	for _, value := range invalid {
		if _, err := NewStreamHeaders(map[string]string{"Origin": value}); !errors.Is(err, ErrUnsafeStreamHeaders) {
			t.Errorf("Origin %q error = %v, want ErrUnsafeStreamHeaders", value, err)
		}
	}
}

func TestNewStreamHeadersRefererShape(t *testing.T) {
	valid := []string{
		"https://www.bilibili.com/",
	}
	for _, value := range valid {
		if _, err := NewStreamHeaders(map[string]string{"Referer": value}); err != nil {
			t.Errorf("Referer %q rejected: %v", value, err)
		}
	}

	invalid := []string{
		"/relative",
		"ftp://example.com/file",
		"http://www.bilibili.com/",
		"https://bilibili.com/",
		"https://www.bilibili.com",
		"https://www.bilibili.com/watch",
		"https://www.bilibili.com/?token=canary",
		"https://www.bilibili.com/?",
		"https://www.bilibili.com/#",
		"https://user:pass@example.com/file",
		"https://example.com/file#fragment",
		"https://example.com/file#",
		"https://example.com:/file",
		"https://example.com:65536/file",
		"https://example.com\\file",
	}
	for _, value := range invalid {
		if _, err := NewStreamHeaders(map[string]string{"Referer": value}); !errors.Is(err, ErrUnsafeStreamHeaders) {
			t.Errorf("Referer %q error = %v, want ErrUnsafeStreamHeaders", value, err)
		}
	}
}

func TestNewStreamHeadersForURLBindsOriginAndProfile(t *testing.T) {
	const directURL = "https://cdn.example.test:8443/video.m4s?signature=opaque"

	got, err := NewStreamHeadersForURL(directURL, map[string]string{
		"Origin":          "https://CDN.example.test:8443",
		"Accept":          "video/mp4, audio/mp4",
		"Accept-Language": "en-US, ja;q=0.8",
	})
	if err != nil {
		t.Fatalf("safe headers rejected: %v", err)
	}
	if got.Origin != "https://CDN.example.test:8443" {
		t.Fatalf("origin = %q", got.Origin)
	}

	if _, err := NewStreamHeadersForURL(directURL, map[string]string{
		"Origin": "https://cdn.example.test:9443",
	}); !errors.Is(err, ErrUnsafeStreamHeaders) {
		t.Fatalf("mismatched origin error = %v, want ErrUnsafeStreamHeaders", err)
	}
	if _, err := NewStreamHeadersForURL(directURL, map[string]string{
		"User-Agent": "custom-agent-with-source-token",
	}); !errors.Is(err, ErrUnsafeStreamHeaders) {
		t.Fatalf("arbitrary user-agent error = %v, want ErrUnsafeStreamHeaders", err)
	}
	if _, err := NewStreamHeadersForURL(directURL, map[string]string{
		"User-Agent": biliUA,
	}); err != nil {
		t.Fatalf("reviewed user-agent rejected: %v", err)
	}
	if _, err := NewStreamHeadersForURL(directURL, map[string]string{
		"Accept": "text/html",
	}); !errors.Is(err, ErrUnsafeStreamHeaders) {
		t.Fatalf("non-media accept error = %v, want ErrUnsafeStreamHeaders", err)
	}
	if _, err := NewStreamHeadersForURL("https://user:pass@cdn.example.test/video", nil); !errors.Is(err, ErrUnsafeStreamHeaders) {
		t.Fatalf("userinfo URL error = %v, want ErrUnsafeStreamHeaders", err)
	}
}

func TestStreamHeadersValidateManualConstruction(t *testing.T) {
	for _, headers := range []StreamHeaders{
		{UserAgent: "bad\r\nInjected: yes"},
		{Origin: "https://example.com/path"},
		{Referer: "https://user@example.com/"},
		{Referer: "https://www.bilibili.com/?token=canary"},
	} {
		if err := headers.Validate(); !errors.Is(err, ErrUnsafeStreamHeaders) {
			t.Errorf("Validate(%#v) = %v, want ErrUnsafeStreamHeaders", headers, err)
		}
	}
}

func TestStreamHeadersRefererErrorDoesNotEchoTokenCanary(t *testing.T) {
	const canary = "referer-token-canary-7c2b"
	headers, err := NewStreamHeaders(map[string]string{
		"Referer": "https://www.bilibili.com/?token=" + canary,
	})
	if !errors.Is(err, ErrUnsafeStreamHeaders) {
		t.Fatalf("error = %v, want ErrUnsafeStreamHeaders", err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("validation error leaked the referer token: %v", err)
	}
	response, err := json.Marshal(map[string]any{"headers": headers})
	if err != nil {
		t.Fatalf("json.Marshal response: %v", err)
	}
	if strings.Contains(string(response), canary) {
		t.Fatalf("rejected headers leaked the referer token in JSON: %s", response)
	}
}

func TestHeaderDTODoesNotExposeArbitraryMap(t *testing.T) {
	typ := reflect.TypeOf(StreamHeaders{})
	if typ.Kind() != reflect.Struct || typ.NumField() != 5 {
		t.Fatalf("StreamHeaders must stay a five-field struct, got %v", typ)
	}
	for _, field := range []string{"Referer", "Origin", "UserAgent", "Accept", "AcceptLanguage"} {
		if _, ok := typ.FieldByName(field); !ok {
			t.Errorf("StreamHeaders is missing field %s", field)
		}
	}
}
