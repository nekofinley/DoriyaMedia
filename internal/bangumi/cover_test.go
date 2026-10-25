package bangumi

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"doriyamedia/internal/config"
)

type coverRoundTripFunc func(*http.Request) (*http.Response, error)

func (f coverRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestFetchCoverRejectsEverythingExceptBangumiImageHost(t *testing.T) {
	tests := []string{
		"https://example.com/cover.jpg",
		"https://evil.lain.bgm.tv/cover.jpg",
		"https://lain.bgm.tv.evil.example/cover.jpg",
		"https://lain.bgm.tv:444/cover.jpg",
		"https://user@lain.bgm.tv/cover.jpg",
		"ftp://lain.bgm.tv/cover.jpg",
		"file:///etc/passwd",
		"//lain.bgm.tv/cover.jpg",
	}
	for _, rawURL := range tests {
		t.Run(rawURL, func(t *testing.T) {
			calls := 0
			svc := &Service{
				Cfg: &config.Config{},
				coverClient: func() *http.Client {
					return &http.Client{Transport: coverRoundTripFunc(func(r *http.Request) (*http.Response, error) {
						calls++
						t.Fatalf("invalid URL reached the network: %s", r.URL)
						return nil, errors.New("unexpected network call")
					})}
				},
			}
			if _, err := svc.FetchCover(t.Context(), rawURL); !errors.Is(err, ErrInvalidCoverURL) {
				t.Fatalf("FetchCover(%q) error = %v, want ErrInvalidCoverURL", rawURL, err)
			}
			if calls != 0 {
				t.Fatalf("FetchCover(%q) made %d network calls", rawURL, calls)
			}
		})
	}
}

func TestFetchCoverReturnsOnlySupportedImage(t *testing.T) {
	body := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F'}
	calls := 0
	svc := &Service{
		Cfg: &config.Config{},
		coverClient: func() *http.Client {
			return &http.Client{Transport: coverRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if got := r.URL.String(); got != "https://lain.bgm.tv/pic/cover/l/42.jpg" {
					t.Errorf("upstream URL = %q", got)
				}
				if got := r.Header.Get("Authorization"); got != "" {
					t.Errorf("cover request forwarded Authorization: %q", got)
				}
				return &http.Response{
					StatusCode:    http.StatusOK,
					Header:        http.Header{"Content-Type": []string{"image/jpeg"}},
					Body:          io.NopCloser(bytes.NewReader(body)),
					ContentLength: int64(len(body)),
					Request:       r,
				}, nil
			})}
		},
	}
	image, err := svc.FetchCover(t.Context(), "https://lain.bgm.tv/pic/cover/l/42.jpg")
	if err != nil {
		t.Fatalf("FetchCover: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if image.ContentType != "image/jpeg" || !bytes.Equal(image.Data, body) {
		t.Fatalf("image = %+v", image)
	}
}

func TestFetchCoverBlocksRedirectsAwayFromBangumi(t *testing.T) {
	calls := 0
	svc := &Service{
		Cfg: &config.Config{},
		coverClient: func() *http.Client {
			return &http.Client{Transport: coverRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{"https://evil.example/cover.jpg"}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    r,
				}, nil
			})}
		},
	}
	_, err := svc.FetchCover(t.Context(), "https://lain.bgm.tv/pic/cover/l/42.jpg")
	if !errors.Is(err, ErrInvalidCoverURL) {
		t.Fatalf("redirect error = %v, want ErrInvalidCoverURL", err)
	}
	if calls != 1 {
		t.Fatalf("redirect fetched %d requests, want only the original request", calls)
	}
}

func TestFetchCoverRejectsNonImageAndOversizedResponses(t *testing.T) {
	t.Run("content type", func(t *testing.T) {
		svc := testCoverService(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/html"}},
				Body:       io.NopCloser(strings.NewReader("<script>alert(1)</script>")),
				Request:    r,
			}, nil
		})
		if _, err := svc.FetchCover(t.Context(), "https://lain.bgm.tv/pic/cover/l/42.jpg"); !errors.Is(err, ErrCoverContentType) {
			t.Fatalf("content type error = %v, want ErrCoverContentType", err)
		}
	})
	t.Run("content length", func(t *testing.T) {
		svc := testCoverService(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        http.Header{"Content-Type": []string{"image/jpeg"}},
				Body:          io.NopCloser(strings.NewReader("")),
				ContentLength: maxCoverBytes + 1,
				Request:       r,
			}, nil
		})
		if _, err := svc.FetchCover(t.Context(), "https://lain.bgm.tv/pic/cover/l/42.jpg"); !errors.Is(err, ErrCoverTooLarge) {
			t.Fatalf("size error = %v, want ErrCoverTooLarge", err)
		}
	})
}

func testCoverService(roundTrip coverRoundTripFunc) *Service {
	return &Service{
		Cfg: &config.Config{},
		coverClient: func() *http.Client {
			return &http.Client{Transport: roundTrip}
		},
	}
}
