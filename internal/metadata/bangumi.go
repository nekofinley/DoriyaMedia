package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Bangumi (bgm.tv) client. Two API generations are in use:
//   - v0 (api.bgm.tv/v0/...): search/subjects (POST), subjects/{id}, episodes
//   - legacy (api.bgm.tv/search/subject/{kw}): used as a fallback for search
//
// All endpoints are public, but a User-Agent is mandatory (requests without
// one are rejected) and an optional personal access token unlocks higher rate
// limits. Every call is routed through the configured local proxy by default
// because api.bgm.tv is commonly DNS-poisoned in mainland China.
type BangumiClient struct {
	Token   string
	APIBase string
	client  *http.Client

	// NoAuthFallback reports that the last call succeeded only after dropping a
	// rejected token (public endpoints need none).
	NoAuthFallback bool
}

// errBGMAuth marks 401/403 responses so a bad token can be retried anonymously.
type errBGMAuth struct{ code int }

func (e errBGMAuth) Error() string {
	return fmt.Sprintf("bgm: 接口拒绝(%d),可能是令牌无效或已被限流", e.code)
}

const bgmUserAgent = "DoriyaMedia/0.1 (https://github.com/doriyamedia)"

// Subject types used by the Bangumi API.
const (
	BgmTypeBook  = 1
	BgmTypeAnime = 2
	BgmTypeMusic = 3
	BgmTypeGame  = 4
	BgmTypeReal  = 6
)

func NewBangumiClient(token, apiBase string, client *http.Client) *BangumiClient {
	if strings.TrimSpace(apiBase) == "" {
		apiBase = "https://api.bgm.tv"
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &BangumiClient{Token: strings.TrimSpace(token), APIBase: strings.TrimRight(apiBase, "/"), client: client}
}

func (c *BangumiClient) Enabled() bool { return c != nil && c.APIBase != "" }

type bgmSubject struct {
	ID       int    `json:"id"`
	Type     int    `json:"type"`
	Name     string `json:"name"`
	NameCN   string `json:"name_cn"`
	Date     string `json:"date"`
	AirDate  string `json:"air_date"`
	Platform string `json:"platform"`
	Summary  string `json:"summary"`
	Image    string `json:"image"`
	Images   struct {
		Large  string `json:"large"`
		Common string `json:"common"`
		Medium string `json:"medium"`
		Small  string `json:"small"`
		Grid   string `json:"grid"`
	} `json:"images"`
	Rating struct {
		Score float64 `json:"score"`
		Rank  int     `json:"rank"`
		Total int     `json:"total"`
	} `json:"rating"`
	Tags []struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	} `json:"tags"`
	Eps           int `json:"eps"`
	TotalEpisodes int `json:"total_episodes"`
	Volumes       int `json:"volumes"`
}

type bgmEpisode struct {
	ID       int     `json:"id"`
	Type     int     `json:"type"`
	Ep       float64 `json:"ep"`
	Sort     float64 `json:"sort"`
	Name     string  `json:"name"`
	NameCN   string  `json:"name_cn"`
	Airdate  string  `json:"airdate"`
	Duration string  `json:"duration"`
}

// BgmSubject is the metadata DoriyaMedia stores from Bangumi.
type BgmSubject struct {
	ID         int
	Name       string // 原名(通常日文)
	NameCN     string // 中文名
	Year       int
	Overview   string
	Rating     float64
	Genres     []string
	PosterURL  string
	EndDate    string
	Episodes   int
	Platform   string
	TotalCount int
}

// BgmEpisodeMeta is per-episode metadata (Chinese + original title, air date).
type BgmEpisodeMeta struct {
	ID       int
	Number   int
	Title    string // 中文优先
	Original string
	AirDate  string
}

func (c *BangumiClient) do(ctx context.Context, method, u string, body []byte, out any) error {
	err := c.doOnce(ctx, method, u, body, out, c.Token != "")
	if err == nil {
		return nil
	}
	// A stale/incorrect token must not break scraping: search/detail/episodes
	// are public, so retry once without the Authorization header.
	var ae errBGMAuth
	if c.Token != "" && errors.As(err, &ae) {
		if err2 := c.doOnce(ctx, method, u, body, out, false); err2 == nil {
			c.NoAuthFallback = true
			return nil
		}
	}
	return err
}

func (c *BangumiClient) doOnce(ctx context.Context, method, u string, body []byte, out any, withToken bool) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", bgmUserAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if withToken && c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == 404 {
		return fmt.Errorf("bgm: 条目不存在(404)")
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return errBGMAuth{code: resp.StatusCode}
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("bgm: HTTP %d %s", resp.StatusCode, truncate(string(data), 160))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Search finds subjects of one type. v0 search is tried first, then the legacy
// search endpoint (which still returns usable results when v0 is blocked).
func (c *BangumiClient) Search(ctx context.Context, keyword string, typ int) ([]BgmSubject, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, fmt.Errorf("bgm: 关键词为空")
	}
	if typ == 0 {
		typ = BgmTypeAnime
	}
	body, _ := json.Marshal(map[string]any{
		"keyword": keyword,
		"filter":  map[string]any{"type": []int{typ}},
	})
	var v0 struct {
		Data  []bgmSubject `json:"data"`
		Total int          `json:"total"`
	}
	err := c.do(ctx, "POST", c.APIBase+"/v0/search/subjects?limit=10", body, &v0)
	if err == nil && len(v0.Data) > 0 {
		return convertSubjects(v0.Data), nil
	}
	legacy, lerr := c.searchLegacy(ctx, keyword, typ)
	if lerr == nil && len(legacy) > 0 {
		return legacy, nil
	}
	if err != nil {
		return nil, err
	}
	// v0 answered successfully (possibly with no hits). A failing legacy
	// fallback is not the answer to this search: the engine did answer, it just
	// had nothing for this spelling — and returning the fallback's 404 here is
	// what stopped a caller from trying another spelling at all.
	return nil, nil
}

func (c *BangumiClient) searchLegacy(ctx context.Context, keyword string, typ int) ([]BgmSubject, error) {
	u := fmt.Sprintf("%s/search/subject/%s?type=%d&responseGroup=large&max_results=10",
		c.APIBase, url.PathEscape(keyword), typ)
	var res struct {
		Results int          `json:"results"`
		List    []bgmSubject `json:"list"`
	}
	if err := c.do(ctx, "GET", u, nil, &res); err != nil {
		return nil, err
	}
	return convertSubjects(res.List), nil
}

func convertSubjects(in []bgmSubject) []BgmSubject {
	out := make([]BgmSubject, 0, len(in))
	for _, s := range in {
		out = append(out, convertSubject(s))
	}
	return out
}

func convertSubject(s bgmSubject) BgmSubject {
	date := orDefaultStr(s.Date, s.AirDate)
	poster := orDefaultStr(s.Images.Large, orDefaultStr(s.Images.Common, s.Image))
	genres := make([]string, 0, 6)
	for _, t := range s.Tags {
		if t.Name == "" || len(genres) >= 6 {
			break
		}
		genres = append(genres, t.Name)
	}
	eps := s.Eps
	if eps == 0 {
		eps = s.TotalEpisodes
	}
	return BgmSubject{
		ID:         s.ID,
		Name:       s.Name,
		NameCN:     s.NameCN,
		Year:       year4(date),
		Overview:   strings.TrimSpace(s.Summary),
		Rating:     s.Rating.Score,
		Genres:     genres,
		PosterURL:  poster,
		EndDate:    date,
		Episodes:   eps,
		Platform:   s.Platform,
		TotalCount: s.Rating.Total,
	}
}

// Subject fetches full detail (rating, tags, summary, image) for one subject.
func (c *BangumiClient) Subject(ctx context.Context, id int) (*BgmSubject, error) {
	var s bgmSubject
	if err := c.do(ctx, "GET", fmt.Sprintf("%s/v0/subjects/%d", c.APIBase, id), nil, &s); err != nil {
		return nil, err
	}
	out := convertSubject(s)
	return &out, nil
}

// Episodes lists main-story episodes (type=0) with Chinese + original titles.
func (c *BangumiClient) Episodes(ctx context.Context, subjectID int) ([]BgmEpisodeMeta, error) {
	const pageSize = 200
	var all []bgmEpisode
	for offset := 0; ; {
		var res struct {
			Data  []bgmEpisode `json:"data"`
			Total int          `json:"total"`
		}
		u := fmt.Sprintf("%s/v0/episodes?subject_id=%d&type=0&limit=%d&offset=%d",
			c.APIBase, subjectID, pageSize, offset)
		if err := c.do(ctx, "GET", u, nil, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Data...)
		if len(res.Data) < pageSize || (res.Total > 0 && len(all) >= res.Total) {
			break
		}
		offset += len(res.Data)
		if offset > 20000 {
			break
		}
	}
	out := make([]BgmEpisodeMeta, 0, len(all))
	for _, e := range all {
		num := int(e.Ep)
		if num == 0 {
			num = int(e.Sort)
		}
		if num <= 0 {
			continue
		}
		out = append(out, BgmEpisodeMeta{
			ID:       e.ID,
			Number:   num,
			Title:    orDefaultStr(e.NameCN, e.Name),
			Original: e.Name,
			AirDate:  e.Airdate,
		})
	}
	return out, nil
}

// DownloadImage fetches a cover image (lain.bgm.tv) through the same client.
func (c *BangumiClient) DownloadImage(ctx context.Context, rawURL string) ([]byte, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, fmt.Errorf("bgm: 图片地址为空")
	}
	// BGM serves covers over http; upgrade so the proxy sees a normal request.
	u := rawURL
	if strings.HasPrefix(u, "http://") {
		u = "https://" + strings.TrimPrefix(u, "http://")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", bgmUserAgent)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("bgm: 封面 HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, err
	}
	if len(data) < 512 {
		return nil, fmt.Errorf("bgm: 封面数据过小(%d 字节)", len(data))
	}
	return data, nil
}

// BgmImageProxyURL rewrites a Bangumi image URL to a size variant
// (l = large原图, c = common 400px, m = medium, s = small).
func BgmImageProxyURL(rawURL, size string) string {
	if rawURL == "" || size == "" {
		return rawURL
	}
	u := strings.Replace(rawURL, "http://", "https://", 1)
	if i := strings.Index(u, "/pic/cover/"); i >= 0 {
		rest := u[i+len("/pic/cover/"):]
		if len(rest) > 2 && rest[1] == '/' {
			return u[:i] + "/pic/cover/" + size + "/" + rest[2:]
		}
	}
	return u
}

func year4(date string) int {
	if len(date) >= 4 {
		if n, err := strconv.Atoi(date[:4]); err == nil {
			return n
		}
	}
	return 0
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
