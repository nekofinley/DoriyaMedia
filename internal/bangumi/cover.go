package bangumi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxCoverBytes  = int64(8 << 20)
	maxCoverURLLen = 2048
)

var (
	ErrInvalidCoverURL  = errors.New("Bangumi 封面地址无效")
	ErrCoverTooLarge    = errors.New("Bangumi 封面超过大小限制")
	ErrCoverContentType = errors.New("Bangumi 封面格式不受支持")
)

// CoverUpstreamError carries only the upstream HTTP status. The response body
// is intentionally discarded because it is not needed by the image route.
type CoverUpstreamError struct {
	Status int
}

func (e *CoverUpstreamError) Error() string {
	return fmt.Sprintf("Bangumi 封面 HTTP %d", e.Status)
}

type CoverImage struct {
	Data        []byte
	ContentType string
}

// FetchCover downloads an image through the same proxy configuration as the
// Bangumi API client. It accepts only Bangumi's image host and revalidates every
// redirect, so this endpoint cannot be used as a general-purpose URL proxy.
func (s *Service) FetchCover(ctx context.Context, rawURL string) (*CoverImage, error) {
	coverURL, err := parseCoverURL(rawURL)
	if err != nil {
		return nil, err
	}
	var base *http.Client
	if s.coverClient != nil {
		base = s.coverClient()
	}
	if base == nil {
		base = bgmHTTPClient(s.Cfg, 20*time.Second)
	}
	client := *base
	previousRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("%w: 重定向次数过多", ErrInvalidCoverURL)
		}
		if err := validateCoverURL(req.URL); err != nil {
			return fmt.Errorf("%w: 重定向目标不是 Bangumi 封面域名", ErrInvalidCoverURL)
		}
		if previousRedirect != nil {
			return previousRedirect(req, via)
		}
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, coverURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCoverURL, err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/jpeg,image/gif,image/*;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &CoverUpstreamError{Status: resp.StatusCode}
	}
	if resp.ContentLength > maxCoverBytes {
		return nil, ErrCoverTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCoverBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxCoverBytes {
		return nil, ErrCoverTooLarge
	}
	contentType := supportedCoverContentType(resp.Header.Get("Content-Type"), data)
	if contentType == "" {
		return nil, ErrCoverContentType
	}
	return &CoverImage{Data: data, ContentType: contentType}, nil
}

func parseCoverURL(rawURL string) (*url.URL, error) {
	if rawURL == "" || len(rawURL) > maxCoverURLLen {
		return nil, ErrInvalidCoverURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCoverURL, err)
	}
	if err := validateCoverURL(u); err != nil {
		return nil, err
	}
	return u, nil
}

func validateCoverURL(u *url.URL) error {
	if u == nil {
		return ErrInvalidCoverURL
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return fmt.Errorf("%w: 仅支持 http/https", ErrInvalidCoverURL)
	}
	if u.User != nil || u.Port() != "" || u.Fragment != "" || u.Opaque != "" {
		return ErrInvalidCoverURL
	}
	if !strings.EqualFold(u.Hostname(), "lain.bgm.tv") {
		return fmt.Errorf("%w: 仅允许 lain.bgm.tv", ErrInvalidCoverURL)
	}
	if u.Path == "" || !strings.HasPrefix(u.Path, "/") {
		return ErrInvalidCoverURL
	}
	return nil
}

func supportedCoverContentType(header string, data []byte) string {
	mediaType, _, _ := mime.ParseMediaType(header)
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if isSupportedCoverType(mediaType) {
		return mediaType
	}
	// Some CDNs label images as a generic byte stream. Sniffing is only used in
	// that case, never to override an explicitly non-image content type.
	if mediaType != "" && mediaType != "application/octet-stream" {
		return ""
	}
	sniffed := strings.ToLower(http.DetectContentType(data))
	if isSupportedCoverType(sniffed) {
		return sniffed
	}
	return ""
}

func isSupportedCoverType(mediaType string) bool {
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/avif":
		return true
	default:
		return false
	}
}
