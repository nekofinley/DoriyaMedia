package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

// catalog_handlers.go: "this anime is on bilibili but not in my library yet" —
// search the source's catalogue, then import the one series the viewer clicked
// while their loading page watches the job.

type catalogHitDTO struct {
	SourceID    int64  `json:"source_id"`
	SourceName  string `json:"source_name"`
	LibraryID   int64  `json:"library_id"`
	LibraryName string `json:"library_name"`
	source.CatalogHit
}

// catalogTarget is a bilibili source that has somewhere to import into: a series
// library mounting that source. A source mounted by several libraries is only
// offered once (the first library wins), because the result must name exactly
// one import destination.
type catalogTarget struct {
	src    *models.Source
	lib    *models.Library
	search source.CatalogSearcher
}

func (s *Server) catalogTargets() []catalogTarget {
	libs, err := s.Store.Libraries()
	if err != nil {
		return nil
	}
	srcs, err := s.Store.Sources()
	if err != nil {
		return nil
	}
	byID := make(map[int64]*models.Source, len(srcs))
	for i := range srcs {
		byID[srcs[i].ID] = &srcs[i]
	}
	seen := map[int64]bool{}
	var out []catalogTarget
	for i := range libs {
		lib := libs[i]
		if lib.Type != "series" {
			continue
		}
		places, _ := s.Store.LibrarySources(lib.ID)
		if len(places) == 0 {
			places = []models.LibrarySource{{LibraryID: lib.ID, SourceID: lib.SourceID, Path: lib.Path}}
		}
		for _, pl := range places {
			src := byID[pl.SourceID]
			if src == nil || src.Type != "bilibili" || seen[src.ID] {
				continue
			}
			drv, err := source.NewDriver(src)
			if err != nil {
				continue
			}
			cs, ok := drv.(source.CatalogSearcher)
			if !ok {
				continue
			}
			seen[src.ID] = true
			libCopy := lib
			out = append(out, catalogTarget{src: src, lib: &libCopy, search: cs})
		}
	}
	return out
}

// GET /api/catalog/search?q=… — finds 番剧 in bilibili's catalogue that the
// library may not contain. Results carry the library they can be imported into.
func (s *Server) handleCatalogSearch(w http.ResponseWriter, r *http.Request, user *models.User) {
	keyword := strings.TrimSpace(r.URL.Query().Get("q"))
	items := make([]catalogHitDTO, 0, 20)
	if len([]rune(keyword)) < 2 {
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	warnings := make([]string, 0, 2)
	for _, t := range s.catalogTargets() {
		hits, err := t.search.SearchCatalog(ctx, keyword, 12)
		if err != nil {
			warnings = append(warnings, t.src.Name+": "+strings.TrimPrefix(err.Error(), "bilibili: "))
			continue
		}
		for _, h := range hits {
			items = append(items, catalogHitDTO{
				SourceID: t.src.ID, SourceName: t.src.Name,
				LibraryID: t.lib.ID, LibraryName: t.lib.Name,
				CatalogHit: h,
			})
		}
	}
	writeJSON(w, 200, map[string]any{"items": items, "warnings": warnings})
}

// importJob is one series import, polled by its loading page.
type importJob struct {
	mu sync.Mutex

	ID         string
	LibraryID  int64
	SourceID   int64
	SeasonID   int64
	Path       string
	Title      string
	Phase      string // walk | organize | meta | done | error
	Error      string
	ItemID     int64
	RequestIDs map[int64]struct{}
	created    time.Time

	prog *models.ScanProgress
}

func (j *importJob) setPhase(phase string) {
	j.mu.Lock()
	j.Phase = phase
	j.mu.Unlock()
}

func (j *importJob) finished() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Phase == "done" || j.Phase == "error"
}

func (j *importJob) fail(err error) {
	j.mu.Lock()
	j.Phase, j.Error = "error", err.Error()
	j.mu.Unlock()
}

func (j *importJob) addRequest(id int64) {
	if id <= 0 {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Phase == "done" || j.Phase == "error" {
		return
	}
	if j.RequestIDs == nil {
		j.RequestIDs = map[int64]struct{}{}
	}
	j.RequestIDs[id] = struct{}{}
}

// complete marks the import done only after any request cards that joined the
// job have been archived server-side. Keeping the job lock while updating the
// rows prevents another request from attaching between archiving and "done".
func (j *importJob) complete(s *Server, itemID int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Phase == "done" || j.Phase == "error" {
		return
	}
	for requestID := range j.RequestIDs {
		if _, err := s.Store.CompleteMediaRequest(requestID, itemID, ""); err != nil {
			log.Printf("import: archive media request %d for item %d failed: %v", requestID, itemID, err)
		}
	}
	j.Phase = "done"
}

func (j *importJob) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := map[string]any{
		"id": j.ID, "phase": j.Phase, "title": j.Title,
		"item_id": j.ItemID, "error": j.Error,
	}
	if j.prog != nil {
		st, _ := j.prog.Snapshot(0)
		out["state"] = st
	}
	return out
}

func newImportID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("imp-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// startImport reuses a still-running job for the same season, so double-clicking
// a card cannot start two walks of the same series.
func (s *Server) startImport(lib *models.Library, src *models.Source, seasonID int64, path, title string, requestID int64) (*importJob, bool) {
	s.importsMu.Lock()
	defer s.importsMu.Unlock()
	if s.imports == nil {
		s.imports = map[string]*importJob{}
	}
	now := time.Now()
	for id, j := range s.imports {
		if now.Sub(j.created) > time.Hour {
			delete(s.imports, id)
		}
		if j.LibraryID == lib.ID && j.SeasonID == seasonID && !j.finished() {
			j.addRequest(requestID)
			return j, false
		}
	}
	j := &importJob{
		ID: newImportID(), LibraryID: lib.ID, SourceID: src.ID, SeasonID: seasonID,
		Path: path, Title: title, Phase: "walk", created: now,
		prog: models.NewScanProgress(lib.ID), RequestIDs: map[int64]struct{}{},
	}
	j.addRequest(requestID)
	s.imports[j.ID] = j
	return j, true
}

// run walks the series subtree, then identifies the show so the detail page the
// viewer lands on already has its cover and synopsis.
func (j *importJob) run(s *Server, lib *models.Library, src *models.Source) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if s.Scanner == nil {
		j.fail(fmt.Errorf("扫描器未就绪"))
		return
	}
	j.prog.Update(func(st *models.ScanState) { st.Current = "正在读取季度与剧集…" })
	itemID, err := s.Scanner.ImportSeries(ctx, lib, src.ID, j.Path, j.prog)
	if err != nil {
		j.fail(err)
		return
	}
	if itemID == 0 {
		j.fail(fmt.Errorf("剧集已建立,但找不到对应的番剧条目"))
		return
	}
	j.mu.Lock()
	j.ItemID = itemID
	j.mu.Unlock()

	if s.Metadata != nil {
		j.setPhase("meta")
		j.prog.Update(func(st *models.ScanState) {
			st.Phase, st.Current = "meta", "正在匹配封面与简介…"
		})
		if err := s.Metadata.RefreshItem(itemID, 0); err != nil {
			// Metadata is a bonus: the series is already playable without it.
			log.Printf("import: refresh metadata for item %d failed: %v", itemID, err)
		}
	}
	j.prog.Update(func(st *models.ScanState) {
		st.Phase, st.Running, st.Current = "done", false, "入库完成"
	})
	j.complete(s, itemID)
}

// POST /api/catalog/import — starts (or joins) the import of one catalogue season.
func (s *Server) handleCatalogImport(w http.ResponseWriter, r *http.Request, user *models.User) {
	var req struct {
		LibraryID int64 `json:"library_id"`
		SourceID  int64 `json:"source_id"`
		SeasonID  int64 `json:"season_id"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if req.LibraryID <= 0 || req.SourceID <= 0 || req.SeasonID <= 0 {
		fail(w, 400, "缺少 library_id / source_id / season_id")
		return
	}
	lib, err := s.Store.LibraryByID(req.LibraryID)
	if err != nil {
		fail(w, 404, "媒体库不存在")
		return
	}
	if lib.Type != "series" {
		fail(w, 400, "只能导入到剧集库")
		return
	}
	src, err := s.Store.SourceByID(req.SourceID)
	if err != nil {
		fail(w, 404, "存储源不存在")
		return
	}
	if src.Type != "bilibili" {
		fail(w, 400, "这个存储源不支持搜索入库")
		return
	}
	if !s.libraryUsesSource(lib, src.ID) {
		fail(w, 400, "这个媒体库没有挂载该存储源")
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
	job, fresh := s.startImport(lib, src, req.SeasonID, path, title, 0)
	if fresh {
		go job.run(s, lib, src)
	}
	writeJSON(w, 200, map[string]any{"job_id": job.ID, "title": title, "path": path})
}

// GET /api/import/{id} — the loading page's poll.
func (s *Server) handleImportStatus(w http.ResponseWriter, r *http.Request, user *models.User) {
	id := r.PathValue("id")
	s.importsMu.Lock()
	job := s.imports[id]
	s.importsMu.Unlock()
	if job == nil {
		fail(w, 404, "入库任务不存在或已过期")
		return
	}
	writeJSON(w, 200, job.snapshot())
}

func (s *Server) libraryUsesSource(lib *models.Library, sourceID int64) bool {
	if places, err := s.Store.LibrarySources(lib.ID); err == nil {
		for _, p := range places {
			if p.SourceID == sourceID {
				return true
			}
		}
		if len(places) > 0 {
			return false
		}
	}
	return lib.SourceID == sourceID
}
