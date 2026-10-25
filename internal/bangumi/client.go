package bangumi

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

const userAgent = "DoriyaMedia/0.1 (https://github.com/doriyamedia)"

// APIError keeps the HTTP status so the sync service can distinguish a dead
// token or a missing mapping from a temporary upstream failure.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	body := strings.TrimSpace(e.Body)
	if body == "" {
		return fmt.Sprintf("Bangumi API HTTP %d", e.Status)
	}
	return fmt.Sprintf("Bangumi API HTTP %d: %s", e.Status, truncate(body, 180))
}

// Client is a Bangumi v0 client scoped to one viewer's personal access token.
// Unlike the metadata client it never retries anonymously: every call here is
// either an identity check or a write to that viewer's collection.
type Client struct {
	Token   string
	APIBase string
	HTTP    *http.Client
}

type Me struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Nickname string `json:"nickname"`
	Avatar   struct {
		Large  string `json:"large"`
		Medium string `json:"medium"`
		Small  string `json:"small"`
	} `json:"avatar"`
}

// CollectionsPage is one page of a viewer's anime collections returned by
// Bangumi. Service maps it into the smaller API-facing model.
type CollectionsPage struct {
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
	Data   []UserCollection `json:"data"`
}

type UserCollection struct {
	SubjectID int               `json:"subject_id"`
	Type      int               `json:"type"`
	EpStatus  int               `json:"ep_status"`
	UpdatedAt string            `json:"updated_at"`
	Subject   CollectionSubject `json:"subject"`
}

type CollectionSubject struct {
	ID         int             `json:"id"`
	Type       int             `json:"type"`
	Name       string          `json:"name"`
	NameCN     string          `json:"name_cn"`
	Summary    string          `json:"short_summary"`
	Date       string          `json:"date"`
	Images     CollectionImage `json:"images"`
	Eps        int             `json:"eps"`
	Score      float64         `json:"score"`
	Rank       int             `json:"rank"`
	Collection int             `json:"collection_total"`
	Tags       []SubjectTag    `json:"tags"`
}

type CollectionImage struct {
	Large  string `json:"large"`
	Common string `json:"common"`
	Medium string `json:"medium"`
	Small  string `json:"small"`
	Grid   string `json:"grid"`
}

type SubjectTag struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func NewClient(token, apiBase string, client *http.Client) *Client {
	if strings.TrimSpace(apiBase) == "" {
		apiBase = "https://api.bgm.tv"
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		Token:   strings.TrimSpace(token),
		APIBase: strings.TrimRight(strings.TrimSpace(apiBase), "/"),
		HTTP:    client,
	}
}

func (c *Client) Me(ctx context.Context) (*Me, error) {
	var me Me
	if err := c.do(ctx, http.MethodGet, "/v0/me", nil, &me); err != nil {
		return nil, err
	}
	if strings.TrimSpace(me.Username) == "" {
		return nil, fmt.Errorf("Bangumi 没有返回用户名")
	}
	return &me, nil
}

// Collections returns one page of the viewer's anime collections. type 0 means
// every collection state; values 1..5 are the Bangumi collection types. The
// endpoint itself is paginated, so callers can request the next page with
// offset rather than retaining every collection in memory.
func (c *Client) Collections(ctx context.Context, username string, collectionType, offset, limit int) (*CollectionsPage, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("Bangumi 用户名无效")
	}
	if collectionType < 0 || collectionType > 5 {
		return nil, fmt.Errorf("Bangumi 收藏类型无效")
	}
	if offset < 0 {
		return nil, fmt.Errorf("Bangumi 收藏分页偏移无效")
	}
	if limit < 1 || limit > 50 {
		return nil, fmt.Errorf("Bangumi 收藏分页大小必须在 1 到 50 之间")
	}
	q := url.Values{}
	q.Set("subject_type", "2") // anime only
	if collectionType != 0 {
		q.Set("type", strconv.Itoa(collectionType))
	}
	q.Set("limit", strconv.Itoa(limit))
	q.Set("offset", strconv.Itoa(offset))
	path := "/v0/users/" + url.PathEscape(username) + "/collections?" + q.Encode()
	var page CollectionsPage
	if err := c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
		return nil, err
	}
	if page.Data == nil {
		page.Data = []UserCollection{}
	}
	return &page, nil
}

// HasCollection checks the viewer's collection before creating one. If the
// subject is already in any collection state, progress sync must preserve that
// state rather than overwriting it with 在看.
func (c *Client) HasCollection(ctx context.Context, username string, subjectID int) (bool, error) {
	if subjectID <= 0 {
		return false, fmt.Errorf("Bangumi 条目 ID 无效")
	}
	path := "/v0/users/" + url.PathEscape(strings.TrimSpace(username)) +
		"/collections/" + strconv.Itoa(subjectID)
	var out map[string]any
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	if err == nil {
		return true, nil
	}
	if ae, ok := err.(*APIError); ok && ae.Status == http.StatusNotFound {
		return false, nil
	}
	return false, err
}

func (c *Client) EnsureCollection(ctx context.Context, username string, subjectID int) error {
	ok, err := c.HasCollection(ctx, username, subjectID)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"type": 3}) // 3 = 在看
	err = c.do(ctx, http.MethodPost, fmt.Sprintf("/v0/users/-/collections/%d", subjectID), body, nil)
	if err == nil {
		return nil
	}
	// Another client may have created the collection between GET and POST. A
	// 400/409 is only a failure if a fresh read still says it is absent.
	var ae *APIError
	if errors.As(err, &ae) && (ae.Status == http.StatusBadRequest || ae.Status == http.StatusConflict) {
		if ok, readErr := c.HasCollection(ctx, username, subjectID); readErr == nil && ok {
			return nil
		}
	}
	return err
}

// MarkWatched marks main-story episode IDs as watched (collection type 2).
func (c *Client) MarkWatched(ctx context.Context, subjectID int, episodeIDs []int) error {
	if subjectID <= 0 {
		return fmt.Errorf("Bangumi 条目 ID 无效")
	}
	if len(episodeIDs) == 0 {
		return fmt.Errorf("没有可同步的 Bangumi 章节")
	}
	body, _ := json.Marshal(map[string]any{
		"episode_id": episodeIDs,
		"type":       2,
	})
	return c.do(ctx, http.MethodPatch,
		fmt.Sprintf("/v0/users/-/collections/%d/episodes", subjectID), body, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	if strings.TrimSpace(c.Token) == "" {
		return fmt.Errorf("还没有设置 Bangumi 个人访问令牌")
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.APIBase+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return &APIError{Status: resp.StatusCode, Body: string(data)}
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("解析 Bangumi 响应失败: %w", err)
	}
	return nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
