package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"doriyamedia/internal/bangumi"
	"doriyamedia/internal/models"
)

// bangumi_handlers.go: the per-user half of Bangumi integration. The existing
// global BGM config is for metadata scraping only; these routes read and write
// the caller's own credential and never expose its token.

func (s *Server) handleBangumiAccount(w http.ResponseWriter, r *http.Request, user *models.User) {
	st, err := s.BangumiSync.Status(user.ID)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, st)
}

func (s *Server) handleBangumiConnect(w http.ResponseWriter, r *http.Request, user *models.User) {
	var req struct {
		Token string `json:"token"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	req.Token = strings.TrimSpace(req.Token)
	if len(req.Token) >= len("Bearer ") && strings.EqualFold(req.Token[:len("Bearer ")], "Bearer ") {
		req.Token = strings.TrimSpace(req.Token[len("Bearer "):])
	}
	if len(req.Token) < 8 || len(req.Token) > 2048 {
		fail(w, 400, "Bangumi 令牌长度不正确")
		return
	}
	st, err := s.BangumiSync.Connect(r.Context(), user.ID, req.Token)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, st)
}

func (s *Server) handleBangumiDisconnect(w http.ResponseWriter, r *http.Request, user *models.User) {
	if err := s.BangumiSync.Disconnect(user.ID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleBangumiSettings(w http.ResponseWriter, r *http.Request, user *models.User) {
	var req struct {
		SyncEnabled *bool `json:"sync_enabled"`
	}
	if err := readBody(r, &req); err != nil || req.SyncEnabled == nil {
		fail(w, 400, "sync_enabled 必填")
		return
	}
	if err := s.BangumiSync.SetSyncEnabled(user.ID, *req.SyncEnabled); err != nil {
		fail(w, 400, err.Error())
		return
	}
	st, err := s.BangumiSync.Status(user.ID)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, st)
}

func (s *Server) handleBangumiBackfill(w http.ResponseWriter, r *http.Request, user *models.User) {
	n, err := s.BangumiSync.Backfill(user.ID)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	st, err := s.BangumiSync.Status(user.ID)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"queued": n, "status": st})
}

func (s *Server) handleBangumiCollections(w http.ResponseWriter, r *http.Request, user *models.User) {
	collectionType, offset, limit, err := bangumiCollectionParams(r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	page, err := s.BangumiSync.Collections(r.Context(), user.ID, collectionType, offset, limit)
	if err != nil {
		if errors.Is(err, bangumi.ErrInvalidCollectionsQuery) {
			fail(w, 400, "Bangumi 收藏分页参数无效")
			return
		}
		if errors.Is(err, bangumi.ErrCollectionsUpstream) {
			fail(w, 502, "读取 Bangumi 收藏失败")
			return
		}
		// Database or credential decryption details belong in local logs, not a
		// response. In particular, this route must never relay upstream bodies
		// that could echo an authorization token.
		fail(w, 500, "读取 Bangumi 收藏失败")
		return
	}
	writeJSON(w, 200, page)
}

func bangumiCollectionParams(r *http.Request) (collectionType, offset, limit int, err error) {
	q := r.URL.Query()
	collectionType, err = parseBangumiQueryInt(q.Get("type"), 0)
	if err != nil || collectionType < 0 || collectionType > 5 {
		return 0, 0, 0, fmt.Errorf("type 必须是 0 到 5 之间的整数")
	}
	offset, err = parseBangumiQueryInt(q.Get("offset"), 0)
	if err != nil || offset < 0 {
		return 0, 0, 0, fmt.Errorf("offset 必须是非负整数")
	}
	limit, err = parseBangumiQueryInt(q.Get("limit"), 50)
	if err != nil || limit < 1 || limit > 50 {
		return 0, 0, 0, fmt.Errorf("limit 必须是 1 到 50 之间的整数")
	}
	return collectionType, offset, limit, nil
}

func parseBangumiQueryInt(raw string, fallback int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	return strconv.Atoi(raw)
}

func (s *Server) handleBangumiCover(w http.ResponseWriter, r *http.Request, _ *models.User) {
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		fail(w, 400, "缺少封面地址")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	image, err := s.BangumiSync.FetchCover(ctx, rawURL)
	if err != nil {
		var upstream *bangumi.CoverUpstreamError
		switch {
		case errors.Is(err, bangumi.ErrInvalidCoverURL):
			fail(w, 400, "封面地址无效")
		case errors.Is(err, bangumi.ErrCoverTooLarge), errors.Is(err, bangumi.ErrCoverContentType):
			fail(w, 502, "Bangumi 返回的封面不受支持")
		case errors.As(err, &upstream) && (upstream.Status == http.StatusNotFound || upstream.Status == http.StatusGone):
			fail(w, 404, "封面不存在")
		case errors.As(err, &upstream) && upstream.Status == http.StatusTooManyRequests:
			fail(w, 429, "Bangumi 请求过于频繁,请稍后再试")
		case errors.Is(err, context.DeadlineExceeded):
			fail(w, 504, "读取 Bangumi 封面超时")
		default:
			fail(w, 502, "读取 Bangumi 封面失败")
		}
		return
	}
	w.Header().Set("Content-Type", image.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(image.Data)))
	w.Header().Set("Cache-Control", "private, max-age=86400, stale-while-revalidate=604800")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(image.Data)
}
