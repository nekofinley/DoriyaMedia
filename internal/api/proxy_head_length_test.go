package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbeSizeByRangeValidatesPartialBody(t *testing.T) {
	tests := []struct {
		name         string
		contentRange string
		length       string
		body         string
		want         bool
		wantTotal    int64
	}{
		{
			name:         "valid",
			contentRange: "bytes 0-0/500",
			length:       "1",
			body:         "x",
			want:         true,
			wantTotal:    500,
		},
		{
			name:         "valid without length header",
			contentRange: "bytes 0-0/500",
			body:         "x",
			want:         true,
			wantTotal:    500,
		},
		{
			name:         "total before range",
			contentRange: "bytes 100-199/50",
			length:       "100",
			body:         strings.Repeat("x", 100),
		},
		{
			name:         "wrong returned range",
			contentRange: "bytes 0-1/500",
			length:       "2",
			body:         "xx",
		},
		{
			name:         "zero total",
			contentRange: "bytes 0-0/0",
			length:       "1",
			body:         "x",
		},
		{
			name:         "claimed length disagrees with range",
			contentRange: "bytes 0-0/500",
			length:       "2",
			body:         "xx",
		},
		{
			name:         "body shorter than one byte",
			contentRange: "bytes 0-0/500",
			length:       "1",
		},
		{
			name:         "body longer than one byte",
			contentRange: "bytes 0-0/500",
			length:       "1",
			body:         "xy",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: playbackProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				header := http.Header{"Content-Type": {"video/mp4"}}
				header.Set("Content-Range", tc.contentRange)
				if tc.length != "" {
					header.Set("Content-Length", tc.length)
				}
				return &http.Response{
					StatusCode:    http.StatusPartialContent,
					Status:        "206 Partial Content",
					Header:        header,
					Body:          io.NopCloser(strings.NewReader(tc.body)),
					ContentLength: -1,
					Request:       req,
				}, nil
			})}

			total, _, ok := probeSizeByRange(
				context.Background(), "https://media.example.test/clip.mp4", nil, client)
			if ok != tc.want {
				t.Fatalf("probe accepted = %v, want %v", ok, tc.want)
			}
			if ok && total != tc.wantTotal {
				t.Errorf("probe total = %d, want %d", total, tc.wantTotal)
			}
		})
	}
}

func TestPlaybackProxyHeadUnknownLengthMapsToSourceUnavailable(t *testing.T) {
	client := &http.Client{Transport: playbackProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		header := http.Header{"Content-Type": {"video/mp4"}}
		if req.Method == http.MethodHead {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Status:        "200 OK",
				Header:        header,
				Body:          io.NopCloser(strings.NewReader("")),
				ContentLength: -1,
				Request:       req,
			}, nil
		}
		header.Set("Content-Range", "bytes 0-0/500")
		header.Set("Content-Length", "2")
		return &http.Response{
			StatusCode:    http.StatusPartialContent,
			Status:        "206 Partial Content",
			Header:        header,
			Body:          io.NopCloser(strings.NewReader("xx")),
			ContentLength: -1,
			Request:       req,
		}, nil
	})}

	rec := runPlaybackProxy(t, http.MethodHead,
		"https://media.example.test/clip.mp4", nil, client, nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD error body length = %d, want 0", rec.Body.Len())
	}
}

func TestLegacyProxyHeadUnknownLengthMapsToBadGateway(t *testing.T) {
	client := &http.Client{Transport: playbackProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		header := http.Header{"Content-Type": {"video/mp4"}}
		if req.Method == http.MethodHead {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Status:        "200 OK",
				Header:        header,
				Body:          io.NopCloser(strings.NewReader("")),
				ContentLength: -1,
				Request:       req,
			}, nil
		}
		header.Set("Content-Range", "bytes 0-0/500")
		header.Set("Content-Length", "2")
		return &http.Response{
			StatusCode:    http.StatusPartialContent,
			Status:        "206 Partial Content",
			Header:        header,
			Body:          io.NopCloser(strings.NewReader("xx")),
			ContentLength: -1,
			Request:       req,
		}, nil
	})}

	req := httptest.NewRequest(http.MethodHead, "/api/stream/1?proxy=1", nil)
	rec := httptest.NewRecorder()
	proxyRemoteVia(rec, req, "https://media.example.test/clip.mp4", nil, client)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", rec.Code, rec.Body.String())
	}
}
