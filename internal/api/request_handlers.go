package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"doriyamedia/internal/metadata"
	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

// request_handlers.go: the viewer-facing wishlist and its administrator board.
// Search results carry enough metadata to draw a poster card before the title
// is submitted, and the chosen metadata is snapshotted into the request row.

type requestSearchHit struct {
	Provider        string  `json:"provider"` // bilibili | bangumi
	Title           string  `json:"title"`
	OriginalTitle   string  `json:"original_title,omitempty"`
	Year            int     `json:"year,omitempty"`
	Cover           string  `json:"cover,omitempty"`
	Overview        string  `json:"overview,omitempty"`
	Rating          float64 `json:"rating,omitempty"`
	Episodes        int     `json:"episodes,omitempty"`
	SourceID        int64   `json:"source_id,omitempty"`
	SourceName      string  `json:"source_name,omitempty"`
	LibraryID       int64   `json:"library_id,omitempty"`
	LibraryName     string  `json:"library_name,omitempty"`
	SeasonID        int64   `json:"season_id,omitempty"`
	BGMID           int     `json:"bgm_id,omitempty"`
	InLibraryItemID int64   `json:"in_library_item_id,omitempty"`
}

type bangumiSearchResult struct {
	items []metadata.BgmSubject
	err   error
}

// GET /api/requests/search?q=… — Bilibili catalogue hits are the actionable
// path (one click can import them); Bangumi supplies metadata and covers when
// the title is absent from Bilibili or no Bilibili source is configured.
func (s *Server) handleRequestSearch(w http.ResponseWriter, r *http.Request, _ *models.User) {
	keyword := strings.TrimSpace(r.URL.Query().Get("q"))
	items := []requestSearchHit{}
	if utf8.RuneCountInString(keyword) < 2 {
		writeJSON(w, 200, map[string]any{"items": items, "warnings": []string{}})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	bgmCh := make(chan bangumiSearchResult, 1)
	if s.Metadata == nil {
		bgmCh <- bangumiSearchResult{err: fmt.Errorf("元数据服务未就绪")}
	} else {
		go func() {
			found, err := s.Metadata.SearchBangumi(ctx, keyword)
			bgmCh <- bangumiSearchResult{items: found, err: err}
		}()
	}

	warnings := []string{}
	seenTitles := map[string]bool{}
	for _, target := range s.catalogTargets() {
		hits, err := target.search.SearchCatalog(ctx, keyword, 12)
		if err != nil {
			warnings = append(warnings, target.src.Name+": "+strings.TrimPrefix(err.Error(), "bilibili: "))
			continue
		}
		for _, hit := range hits {
			title := strings.TrimSpace(hit.Title)
			if title == "" {
				continue
			}
			seenTitles[requestTitleKey(title)] = true
			seenTitles[requestTitleKey(hit.Original)] = true
			items = append(items, requestSearchHit{
				Provider:      "bilibili",
				Title:         title,
				OriginalTitle: strings.TrimSpace(hit.Original),
				Year:          hit.Year,
				Cover:         requestCoverURL(hit.Cover),
				Overview:      strings.TrimSpace(hit.Overview),
				Rating:        hit.Score,
				Episodes:      hit.Episodes,
				SourceID:      target.src.ID,
				SourceName:    target.src.Name,
				LibraryID:     target.lib.ID,
				LibraryName:   target.lib.Name,
				SeasonID:      hit.SeasonID,
			})
		}
	}

	bgm := <-bgmCh
	if bgm.err != nil {
		warnings = append(warnings, "Bangumi: "+bgm.err.Error())
	} else {
		for _, sub := range bgm.items {
			title := strings.TrimSpace(sub.NameCN)
			if title == "" {
				title = strings.TrimSpace(sub.Name)
			}
			if title == "" || seenTitles[requestTitleKey(title)] {
				continue
			}
			original := strings.TrimSpace(sub.Name)
			if original == title {
				original = ""
			}
			items = append(items, requestSearchHit{
				Provider:      "bangumi",
				Title:         title,
				OriginalTitle: original,
				Year:          sub.Year,
				Cover:         requestCoverURL(metadata.BgmImageProxyURL(sub.PosterURL, "c")),
				Overview:      strings.TrimSpace(sub.Overview),
				Rating:        sub.Rating,
				Episodes:      sub.Episodes,
				BGMID:         sub.ID,
			})
			if len(items) >= 24 {
				break
			}
		}
	}

	localByTitle, localByBGM := s.requestLibraryIndex(keyword)
	for i := range items {
		if items[i].BGMID > 0 {
			items[i].InLibraryItemID = localByBGM[items[i].BGMID]
		}
		if items[i].InLibraryItemID == 0 {
			items[i].InLibraryItemID = localByTitle[requestTitleKey(items[i].Title)]
		}
	}
	writeJSON(w, 200, map[string]any{
		"items": items, "warnings": uniqueStrings(warnings),
	})
}

// requestLibraryIndex is a small local search used to label hits already owned
// by the library. It only loads roots, so an anime with many episodes does not
// turn one lookup into hundreds of rows.
func (s *Server) requestLibraryIndex(keyword string) (map[string]int64, map[int]int64) {
	byTitle := map[string]int64{}
	byBGM := map[int]int64{}
	libs, err := s.Store.Libraries()
	if err != nil {
		return byTitle, byBGM
	}
	for _, lib := range libs {
		if strings.HasPrefix(lib.Name, "__browse__") {
			continue
		}
		items, _, err := s.Store.ItemsByLibrary(lib.ID, "", keyword, "name", true, 0, 100)
		if err != nil {
			continue
		}
		for _, it := range items {
			if it.Type != "show" && it.Type != "movie" {
				continue
			}
			for _, title := range []string{it.Name, it.OriginalTitle} {
				if key := requestTitleKey(title); key != "" {
					byTitle[key] = it.ID
				}
			}
			if it.BGMID > 0 {
				byBGM[it.BGMID] = it.ID
			}
		}
	}
	return byTitle, byBGM
}

// GET /api/requests — the caller's own board, newest first.
func (s *Server) handleListMediaRequests(w http.ResponseWriter, r *http.Request, user *models.User) {
	items, err := s.Store.MediaRequests(user.ID, "")
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// POST /api/requests — snapshots a selected search result or a manual title.
func (s *Server) handleCreateMediaRequest(w http.ResponseWriter, r *http.Request, user *models.User) {
	var req struct {
		Provider      string  `json:"provider"`
		Title         string  `json:"title"`
		OriginalTitle string  `json:"original_title"`
		Year          int     `json:"year"`
		CoverURL      string  `json:"cover_url"`
		Overview      string  `json:"overview"`
		Rating        float64 `json:"rating"`
		Episodes      int     `json:"episodes"`
		SourceID      int64   `json:"source_id"`
		LibraryID     int64   `json:"library_id"`
		SeasonID      int64   `json:"season_id"`
		BGMID         int     `json:"bgm_id"`
		Note          string  `json:"note"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}

	req.Provider = strings.ToLower(strings.TrimSpace(req.Provider))
	req.Title = strings.TrimSpace(req.Title)
	req.OriginalTitle = strings.TrimSpace(req.OriginalTitle)
	req.Overview = strings.TrimSpace(req.Overview)
	req.Note = strings.TrimSpace(req.Note)
	if req.Provider == "" {
		req.Provider = "manual"
	}
	if req.Title == "" {
		fail(w, 400, "请填写番剧名称")
		return
	}
	if utf8.RuneCountInString(req.Title) > 200 || utf8.RuneCountInString(req.OriginalTitle) > 200 {
		fail(w, 400, "番剧名称过长")
		return
	}
	if utf8.RuneCountInString(req.Overview) > 4000 {
		fail(w, 400, "简介过长")
		return
	}
	if utf8.RuneCountInString(req.Note) > 500 {
		fail(w, 400, "备注最多 500 个字符")
		return
	}
	if req.Year < 0 || req.Year > 3000 || req.Episodes < 0 || req.Episodes > 100000 {
		fail(w, 400, "年份或集数不正确")
		return
	}
	if req.Rating < 0 || req.Rating > 10 {
		fail(w, 400, "评分必须在 0 到 10 之间")
		return
	}
	cover, ok := cleanRequestURL(req.CoverURL)
	if !ok {
		fail(w, 400, "封面地址不是有效的 HTTP(S) 链接")
		return
	}

	switch req.Provider {
	case "bilibili":
		if req.SourceID <= 0 || req.LibraryID <= 0 || req.SeasonID <= 0 {
			fail(w, 400, "B 站条目缺少入库目标")
			return
		}
		if cover != "" && !requestCoverAllowed(cover, req.Provider) {
			fail(w, 400, "B 站封面地址来源不受支持")
			return
		}
		lib, err := s.Store.LibraryByID(req.LibraryID)
		if err != nil || lib.Type != "series" {
			fail(w, 400, "目标剧集库不存在")
			return
		}
		src, err := s.Store.SourceByID(req.SourceID)
		if err != nil || src.Type != "bilibili" || !s.libraryUsesSource(lib, src.ID) {
			fail(w, 400, "目标 B 站源或媒体库不可用")
			return
		}
	case "bangumi":
		if req.BGMID <= 0 {
			fail(w, 400, "Bangumi 条目 ID 无效")
			return
		}
		if cover != "" && !requestCoverAllowed(cover, req.Provider) {
			fail(w, 400, "Bangumi 封面地址来源不受支持")
			return
		}
		req.SourceID, req.LibraryID, req.SeasonID = 0, 0, 0
	case "manual":
		req.SourceID, req.LibraryID, req.SeasonID, req.BGMID = 0, 0, 0, 0
		cover = ""
	default:
		fail(w, 400, "未知的条目来源")
		return
	}

	item := &models.MediaRequest{
		UserID: user.ID, UserName: user.Name, Status: models.MediaRequestPending,
		Provider: req.Provider, Title: req.Title, OriginalTitle: req.OriginalTitle,
		Year: req.Year, CoverURL: cover, Overview: req.Overview, Rating: req.Rating,
		Episodes: req.Episodes, SourceID: req.SourceID, LibraryID: req.LibraryID,
		SeasonID: req.SeasonID, BGMID: req.BGMID, Note: req.Note,
	}
	saved, err := s.Store.CreateMediaRequest(item)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"request": saved})
}

// GET /api/admin/requests — both poster-wall pages in one response, so switching
// tabs never waits on a second round trip.
func (s *Server) handleAdminListMediaRequests(w http.ResponseWriter, r *http.Request, _ *models.User) {
	items, err := s.Store.MediaRequests(0, "")
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	pending := make([]models.MediaRequest, 0, len(items))
	completed := make([]models.MediaRequest, 0, len(items))
	for _, item := range items {
		switch item.Status {
		case models.MediaRequestPending:
			pending = append(pending, item)
		case models.MediaRequestCompleted:
			completed = append(completed, item)
		}
	}
	writeJSON(w, 200, map[string]any{"pending": pending, "completed": completed})
}

// PUT /api/admin/requests/{id} — complete/reopen a request. This is also the
// endpoint the import page calls once a Bilibili import has actually succeeded.
func (s *Server) handleUpdateMediaRequest(w http.ResponseWriter, r *http.Request, _ *models.User) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "求片 ID 无效")
		return
	}
	current, err := s.Store.MediaRequestByID(id)
	if err != nil {
		fail(w, 404, "求片不存在")
		return
	}
	var req struct {
		Status    string  `json:"status"`
		ItemID    *int64  `json:"item_id"`
		AdminNote *string `json:"admin_note"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	adminNote := current.AdminNote
	if req.AdminNote != nil {
		adminNote = strings.TrimSpace(*req.AdminNote)
		if utf8.RuneCountInString(adminNote) > 1000 {
			fail(w, 400, "管理员备注最多 1000 个字符")
			return
		}
	}
	itemID := current.ItemID
	if req.ItemID != nil && *req.ItemID > 0 {
		if _, err := s.Store.ItemByID(*req.ItemID); err != nil {
			fail(w, 400, "关联的媒体条目不存在")
			return
		}
		itemID = *req.ItemID
	}

	switch strings.TrimSpace(req.Status) {
	case models.MediaRequestCompleted:
		current, err = s.Store.CompleteMediaRequest(id, itemID, adminNote)
	case models.MediaRequestPending:
		current, err = s.Store.ReopenMediaRequest(id)
	default:
		fail(w, 400, "状态必须是 pending 或 completed")
		return
	}
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"request": current})
}

func (s *Server) handleDeleteMediaRequest(w http.ResponseWriter, r *http.Request, _ *models.User) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "求片 ID 无效")
		return
	}
	if err := s.Store.DeleteMediaRequest(id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// POST /api/admin/requests/{id}/import — starts the same catalogue import used
// by search-to-import, but from a request card. The job archives the request
// server-side once a real library item exists, even if this page is closed.
func (s *Server) handleRequestImport(w http.ResponseWriter, r *http.Request, _ *models.User) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "求片 ID 无效")
		return
	}
	req, err := s.Store.MediaRequestByID(id)
	if err != nil {
		fail(w, 404, "求片不存在")
		return
	}
	if req.Status != models.MediaRequestPending {
		fail(w, 400, "这条求片已经处理，不能重复入库")
		return
	}
	if req.Provider != "bilibili" || req.SourceID <= 0 || req.LibraryID <= 0 || req.SeasonID <= 0 {
		fail(w, 400, "这条求片没有可用的 B 站入库目标")
		return
	}
	lib, err := s.Store.LibraryByID(req.LibraryID)
	if err != nil || lib.Type != "series" {
		fail(w, 400, "目标剧集库不存在")
		return
	}
	src, err := s.Store.SourceByID(req.SourceID)
	if err != nil || src.Type != "bilibili" || !s.libraryUsesSource(lib, src.ID) {
		fail(w, 400, "目标 B 站源或媒体库不可用")
		return
	}
	drv, err := source.NewDriver(src)
	if err != nil {
		fail(w, 500, "存储源配置错误: "+err.Error())
		return
	}
	cs, ok := drv.(source.CatalogSearcher)
	if !ok {
		fail(w, 400, "这个存储源不支持搜索入库")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	path, title, err := cs.CatalogImportPath(ctx, req.SeasonID)
	cancel()
	if err != nil {
		fail(w, 502, "解析番剧失败: "+err.Error())
		return
	}
	job, fresh := s.startImport(lib, src, req.SeasonID, path, title, req.ID)
	if fresh {
		go job.run(s, lib, src)
	} else if job.finished() {
		// A job that finished between the request lookup and startImport must
		// still archive this card; otherwise it would wait forever.
		if _, err := s.Store.CompleteMediaRequest(req.ID, job.ItemID, ""); err != nil {
			fail(w, 500, "求片归档失败: "+err.Error())
			return
		}
	}
	writeJSON(w, 200, map[string]any{
		"job_id": job.ID, "title": title, "path": path, "request_id": req.ID,
	})
}

func requestCoverURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "http://") {
		raw = "https://" + strings.TrimPrefix(raw, "http://")
	}
	if strings.Contains(raw, "hdslb.com") && !strings.Contains(raw, "@") {
		raw += "@320w_480h_1c.webp"
	}
	return raw
}

func cleanRequestURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	if len(raw) > 2048 {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return raw, true
}

func requestCoverAllowed(raw, provider string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	var domains []string
	switch provider {
	case "bilibili":
		domains = []string{"hdslb.com", "bilibili.com", "biliimg.com"}
	case "bangumi":
		domains = []string{"bgm.tv", "bangumi.tv"}
	default:
		return false
	}
	for _, domain := range domains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func requestTitleKey(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range raw {
		if unicode.IsSpace(r) || strings.ContainsRune("·・:：!！?？._-—–()（）[]【】《》「」/\\", r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func uniqueStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}
