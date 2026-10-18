package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"doriyamedia/internal/models"
	"doriyamedia/internal/scan"
	"doriyamedia/internal/source"
)

// GET /api/browse/{sourceId}?path=/ — list a directory of a source (file browser).
func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request, user *models.User) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	src, err := s.Store.SourceByID(id)
	if err != nil {
		fail(w, 404, "存储源不存在")
		return
	}
	drv, err := source.NewDriver(src)
	if err != nil {
		fail(w, 500, "存储源配置错误: "+err.Error())
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	entries, err := drv.List(ctx, path)
	if err != nil {
		fail(w, 502, "列出目录失败: "+err.Error())
		return
	}
	type entryDTO struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size"`
		Video bool   `json:"video"`
	}
	out := make([]entryDTO, 0, len(entries))
	for _, e := range entries {
		if scan.SkipEntry(e.Name) {
			continue
		}
		out = append(out, entryDTO{
			Name: e.Name, Path: e.Path, IsDir: e.IsDir, Size: e.Size,
			Video: !e.IsDir && scan.IsVideo(e.Name),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, 200, map[string]any{"path": path, "name": src.Name, "entries": out})
}

// GET /api/browseplay/{sourceId}?path=/x/y.mp4 — play any video file without
// creating/scanning a library. The file is registered into a hidden
// "__browse__" library, so the whole decision/HLS/progress pipeline works as-is.
func (s *Server) handleBrowsePlay(w http.ResponseWriter, r *http.Request, user *models.User) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" {
		fail(w, 400, "缺少 path 参数")
		return
	}
	if _, err := s.Store.SourceByID(id); err != nil {
		fail(w, 404, "存储源不存在")
		return
	}
	file, err := s.ensureBrowseFile(r.Context(), id, p)
	if err != nil {
		log.Printf("browse play ensure failed: %v", err)
		fail(w, 500, err.Error())
		return
	}
	s.playFile(w, r, file)
}

func (s *Server) ensureBrowseFile(ctx context.Context, sourceID int64, path string) (*models.MediaFile, error) {
	// Already scanned into a library? Play that row as-is — re-registering it
	// under the browse scratchpad would steal the file from its show/movie.
	if f, err := s.Store.FileBySourcePath(sourceID, path); err == nil {
		return f, nil
	}
	lib, err := s.Store.EnsureBrowseLibrary(sourceID)
	if err != nil {
		return nil, err
	}
	if f, err := s.Store.FileByPath(lib.ID, path); err == nil {
		return f, nil
	}
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	pn := scan.ParseName(base)
	name := pn.Title
	if pn.Kind == "episode" {
		name = fmt.Sprintf("%s S%02dE%02d", pn.Title, pn.Season, pn.Episode)
	}
	if strings.TrimSpace(name) == "" {
		name = base
	}
	item := &models.Item{LibraryID: lib.ID, Type: "movie", Name: name, Year: pn.Year}
	if err := s.Store.InsertItem(item); err != nil {
		return nil, err
	}
	f := &models.MediaFile{ItemID: item.ID, SourceID: sourceID, Path: path}
	id, err := s.Store.UpsertFile(f)
	if err != nil {
		return nil, err
	}
	// UpsertFile returns the row id instead of writing it back into the struct,
	// and everything downstream builds its URLs from file.ID: without this the
	// first playback of a file would ask for /api/hls/0/… — a file that does not
	// exist. Every later request found the row by path and had an id, which is
	// why only the first click was broken.
	f.ID = id
	return f, nil
}
