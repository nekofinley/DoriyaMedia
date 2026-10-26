package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"doriyamedia/internal/models"
	"doriyamedia/internal/netproxy"
	"doriyamedia/internal/source"
)

// ---------- sources ----------

type sourceDTO struct {
	models.Source
	Config models.SourceConfig `json:"config"`
}

// maskedSecret replaces a stored credential on the way out to a non-admin.
const maskedSecret = "******"

func toSourceDTO(src models.Source) sourceDTO {
	var cfg models.SourceConfig
	_ = jsonUnmarshal(src.Config, &cfg)
	return sourceDTO{Source: src, Config: cfg}
}

// maskSourceConfig blanks the credentials in a config. GET /api/sources is open
// to every signed-in user (the browse page lists sources), so without this any
// account on the server could read a WebDAV password, an OpenList token or a
// bilibili cookie — and a bilibili cookie is a whole account, membership
// included. Non-secret fields stay: the file browser shows a source's address.
func maskSourceConfig(cfg models.SourceConfig) models.SourceConfig {
	if cfg.WebDAVPass != "" {
		cfg.WebDAVPass = maskedSecret
	}
	if cfg.Token != "" {
		cfg.Token = maskedSecret
	}
	if cfg.BiliCookie != "" {
		cfg.BiliCookie = maskedSecret
	}
	// The refresh token mints cookies, so it is the same credential as the cookie
	// and gets the same treatment.
	if cfg.BiliRefreshToken != "" {
		cfg.BiliRefreshToken = maskedSecret
	}
	// A proxy string carries a password, and for Shadowsocks the password *is*
	// the key: masking it is the same rule as the cookie.
	if cfg.BiliProxy != "" {
		if spec, err := netproxy.Parse(cfg.BiliProxy); err == nil {
			cfg.BiliProxy = spec.Display()
		} else {
			cfg.BiliProxy = maskedSecret
		}
	}
	return cfg
}

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request, user *models.User) {
	srcs, err := s.Store.Sources()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	admin := user != nil && user.IsAdmin()
	out := make([]sourceDTO, 0, len(srcs))
	for _, src := range srcs {
		dto := toSourceDTO(src)
		if !admin {
			dto.Config = maskSourceConfig(dto.Config)
		}
		out = append(out, dto)
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request, user *models.User) {
	var req struct {
		Name   string              `json:"name"`
		Type   string              `json:"type"`
		Config models.SourceConfig `json:"config"`
	}
	if err := readBody(r, &req); err != nil || req.Name == "" {
		fail(w, 400, "名称不能为空")
		return
	}
	if req.Type != "local" && req.Type != "webdav" && req.Type != "openlist" && req.Type != "bilibili" {
		fail(w, 400, "类型必须是 local/webdav/openlist/bilibili")
		return
	}
	raw, _ := jsonMarshal(req.Config)
	src := &models.Source{Name: req.Name, Type: req.Type, Config: string(raw)}
	if err := s.Store.CreateSource(src); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, toSourceDTO(*src))
}

func (s *Server) handleUpdateSource(w http.ResponseWriter, r *http.Request, user *models.User) {
	id, _ := pathID(r)
	src, err := s.Store.SourceByID(id)
	if err != nil {
		fail(w, 404, "源不存在")
		return
	}
	var req struct {
		Name   string              `json:"name"`
		Config models.SourceConfig `json:"config"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, err.Error())
		return
	}
	if req.Name != "" {
		src.Name = req.Name
	}
	cfg := req.Config
	// A masked placeholder means "I could not read this", never "set the secret
	// to ******": the edit form round-trips a config it was given, and writing
	// the mask back would lock the source out of its own account. An empty
	// value is taken at face value — an administrator clearing the box wants
	// the field cleared (that is how a proxy is switched off).
	var old models.SourceConfig
	_ = jsonUnmarshal(src.Config, &old)
	if cfg.BiliCookie == maskedSecret {
		cfg.BiliCookie = old.BiliCookie
	}
	// A scan-to-login stores a refresh token; the edit form knows nothing about
	// it, so it has to survive a save that only changes the codec — otherwise
	// every settings edit would silently turn a renewable session back into a
	// pasted cookie that expires.
	if cfg.BiliRefreshToken == "" || cfg.BiliRefreshToken == maskedSecret {
		cfg.BiliRefreshToken = old.BiliRefreshToken
	}
	if cfg.BiliProxy == maskedSecret {
		cfg.BiliProxy = old.BiliProxy
	}
	if cfg.Token == maskedSecret {
		cfg.Token = old.Token
	}
	if cfg.WebDAVPass == maskedSecret {
		cfg.WebDAVPass = old.WebDAVPass
	}
	raw, _ := jsonMarshal(cfg)
	src.Config = string(raw)
	if err := s.Store.UpdateSource(src); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, toSourceDTO(*src))
}

func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request, user *models.User) {
	id, _ := pathID(r)
	if err := s.Store.DeleteSource(id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleTestSource validates source config by listing its root.
func (s *Server) handleTestSource(w http.ResponseWriter, r *http.Request, user *models.User) {
	var req struct {
		Type   string              `json:"type"`
		Config models.SourceConfig `json:"config"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, err.Error())
		return
	}
	var drv source.Driver
	var err error
	switch req.Type {
	case "local":
		drv = source.NewLocal(req.Config.Root)
	case "webdav":
		drv = source.NewWebDAV(req.Config.WebDAVURL, req.Config.WebDAVUser, req.Config.WebDAVPass)
	case "openlist":
		drv, err = source.NewOpenList(req.Config.BaseURL, req.Config.Token)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
	case "bilibili":
		drv, err = source.NewBilibili(req.Config.BiliCookie, req.Config.BiliCodec, req.Config.BiliProxy, req.Config.BiliPlayableOnly)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
	default:
		fail(w, 400, "未知类型")
		return
	}
	ctx := r.Context()
	root := "/"
	if req.Type == "local" {
		root = "/"
	}
	// A bilibili source that needs a proxy is usually configured for one reason:
	// the region check. Answer that question in the same reply, so the settings
	// page can say "this proxy is seen in China" or "…in the US, 番剧 will be
	// refused" without a second round trip and without the user having to play
	// an episode to find out.
	var region *netproxy.Check
	if req.Type == "bilibili" && strings.TrimSpace(req.Config.BiliProxy) != "" {
		if spec, err := netproxy.Parse(req.Config.BiliProxy); err == nil {
			c := netproxy.CheckProxy(spec, 20*time.Second)
			region = &c
		}
	}
	entries, err := drv.List(ctx, root)
	if err != nil {
		// A failed test still answers 200 with ok:false. The region check is the
		// diagnosis that makes the failure actionable ("your proxy works but it
		// lands in Japan"), and a 4xx would make the settings page throw the
		// body away and show only the error string.
		resp := map[string]any{"ok": false, "error": "连接失败: " + err.Error()}
		if region != nil {
			resp["region"] = region
		}
		writeJSON(w, 200, resp)
		return
	}
	names := make([]string, 0, 10)
	for i, e := range entries {
		if i >= 10 {
			break
		}
		if e.IsDir {
			names = append(names, "[目录] "+e.Name)
		} else {
			names = append(names, e.Name)
		}
	}
	resp := map[string]any{"ok": true, "count": len(entries), "entries": names}
	if region != nil {
		resp["region"] = region
	}
	writeJSON(w, 200, resp)
}

// handleTestProxy checks a proxy setting on its own, before a source exists.
// The settings form asks this while the user is still typing, so it takes the
// bare string rather than a whole source config.
func (s *Server) handleTestProxy(w http.ResponseWriter, r *http.Request, user *models.User) {
	var req struct {
		Proxy string `json:"proxy"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, err.Error())
		return
	}
	spec, err := netproxy.Parse(req.Proxy)
	if errors.Is(err, netproxy.ErrNoProxy) {
		// Empty means direct: report where this server itself is seen.
		writeJSON(w, 200, netproxy.CheckProxy(netproxy.Spec{}, 20*time.Second))
		return
	}
	if err != nil {
		writeJSON(w, 400, netproxy.Check{OK: false, Err: err.Error(), Message: err.Error()})
		return
	}
	writeJSON(w, 200, netproxy.CheckProxy(spec, 20*time.Second))
}

// ---------- libraries ----------

func (s *Server) handleListLibraries(w http.ResponseWriter, r *http.Request, user *models.User) {
	libs, err := s.Store.Libraries()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	// hide the file-browser scratchpad libraries
	visible := libs[:0]
	for _, lib := range libs {
		if !strings.HasPrefix(lib.Name, "__browse__") {
			visible = append(visible, lib)
		}
	}
	if visible == nil {
		visible = []models.Library{}
	}
	writeJSON(w, 200, visible)
}

// providersField accepts both shapes clients may send for the scraper list:
// a JSON array (["nfo","bgm","tmdb"]) or the double-encoded string the DB
// stores ("[\"nfo\",\"bgm\",\"tmdb\"]"). Stored form is always the string.
type providersField string

func (p *providersField) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	switch {
	case s == "" || s == "null":
		*p = ""
		return nil
	case strings.HasPrefix(s, `"`):
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		*p = providersField(str)
		return nil
	default:
		var arr []string
		if err := json.Unmarshal(b, &arr); err != nil {
			return err
		}
		raw, _ := json.Marshal(arr)
		*p = providersField(string(raw))
		return nil
	}
}

// excludesField accepts the exclusion list either as a json array or as the
// newline-separated text a form sends, and stores it as json — the column holds
// json, and a caller should not have to know that.
type excludesField string

func (e *excludesField) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	switch {
	case s == "" || s == "null":
		*e = ""
		return nil
	case strings.HasPrefix(s, `"`):
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		*e = excludesField(encodeKeywords(splitKeywords(str)))
		return nil
	default:
		var arr []string
		if err := json.Unmarshal(b, &arr); err != nil {
			return err
		}
		*e = excludesField(encodeKeywords(arr))
		return nil
	}
}

// splitKeywords reads the list the way people type it: one per line, or comma
// separated.
func splitKeywords(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == ',' || r == '，' })
}

// encodeKeywords normalises the list: trimmed, empty entries dropped, and always
// valid json — including for an empty list, which is how a library opts out.
func encodeKeywords(list []string) string {
	raw, _ := json.Marshal(cleanKeywords(list))
	return string(raw)
}

// cleanKeywords trims a list and drops what is not a keyword, so a stray blank
// line in the settings form does not become an exclusion that matches everything.
func cleanKeywords(list []string) []string {
	out := make([]string, 0, len(list))
	for _, k := range list {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

func (s *Server) handleCreateLibrary(w http.ResponseWriter, r *http.Request, user *models.User) {
	var req struct {
		models.Library
		Providers providersField `json:"providers"`
		Excludes  excludesField  `json:"excludes"`
	}
	if err := readBody(r, &req); err != nil || req.Name == "" {
		fail(w, 400, "名称不能为空")
		return
	}
	lib := req.Library
	lib.Providers = string(req.Providers)
	if lib.Type != "movie" && lib.Type != "series" {
		lib.Type = "movie"
	}
	if lib.Path == "" {
		lib.Path = "/"
	}
	if lib.Providers == "" {
		lib.Providers = `["nfo","bgm","tmdb"]`
	}
	// A new library follows the global exclusion list unless it was given one.
	lib.Excludes = string(req.Excludes)
	if lib.ExcludeMode != "custom" {
		lib.ExcludeMode = "inherit"
		lib.Excludes = ""
	}
	// The library's own source_id/path name its first place: the column is NOT
	// NULL with a foreign key, and everything that predates multi-source reads
	// it. A request that sends only the list still has to fill it in.
	if len(lib.Sources) > 0 && lib.SourceID == 0 {
		lib.SourceID = lib.Sources[0].SourceID
		if lib.Sources[0].Path != "" {
			lib.Path = lib.Sources[0].Path
		}
	}
	if err := s.Store.CreateLibrary(&lib); err != nil {
		fail(w, 500, err.Error())
		return
	}
	// A library may scan several places. The first of them is also its own
	// source_id/path, which is what the older single-source shape reads.
	if len(req.Sources) > 0 {
		if err := s.Store.SetLibrarySources(lib.ID, req.Sources); err != nil {
			fail(w, 500, err.Error())
			return
		}
		if fresh, err := s.Store.LibraryByID(lib.ID); err == nil {
			lib = *fresh
		}
	}
	writeJSON(w, 200, lib)
}

func (s *Server) handleUpdateLibrary(w http.ResponseWriter, r *http.Request, user *models.User) {
	id, _ := pathID(r)
	lib, err := s.Store.LibraryByID(id)
	if err != nil {
		fail(w, 404, "媒体库不存在")
		return
	}
	var req struct {
		models.Library
		Providers providersField `json:"providers"`
		Excludes  excludesField  `json:"excludes"`
	}
	if err := readBody(r, &req); err != nil {
		fail(w, 400, err.Error())
		return
	}
	if req.Name != "" {
		lib.Name = req.Name
	}
	if req.Type == "movie" || req.Type == "series" {
		lib.Type = req.Type
	}
	if req.Path != "" {
		lib.Path = req.Path
	}
	if p := string(req.Providers); p != "" {
		lib.Providers = p
	}
	// The exclusion list only changes when the request carries the mode: a client
	// that never heard of the field must not silently reset a tuned library. An
	// empty list is a real answer ("this library excludes nothing"), so it is
	// written, not ignored.
	if req.ExcludeMode != "" {
		lib.ExcludeMode = req.ExcludeMode
	}
	if lib.ExcludeMode == "custom" {
		lib.Excludes = string(req.Excludes)
		if strings.TrimSpace(lib.Excludes) == "" {
			lib.Excludes = "[]"
		}
	} else {
		lib.ExcludeMode = "inherit"
		lib.Excludes = "[]"
	}
	if err := s.Store.UpdateLibrary(lib); err != nil {
		fail(w, 500, err.Error())
		return
	}
	// The sources are replaced wholesale when the request carries them; a request
	// that does not (an older client) leaves them exactly as they were.
	if req.Sources != nil {
		if err := s.Store.SetLibrarySources(lib.ID, req.Sources); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}
	if fresh, err := s.Store.LibraryByID(lib.ID); err == nil {
		lib = fresh
	}
	writeJSON(w, 200, lib)
}

func (s *Server) handleDeleteLibrary(w http.ResponseWriter, r *http.Request, user *models.User) {
	id, _ := pathID(r)
	if err := s.Store.DeleteLibrary(id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleScanLibrary(w http.ResponseWriter, r *http.Request, user *models.User) {
	id, _ := pathID(r)
	lib, err := s.Store.LibraryByID(id)
	if err != nil {
		fail(w, 404, "媒体库不存在")
		return
	}
	go func() {
		_ = s.Scanner.ScanLibrary(lib)
	}()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleStopScan(w http.ResponseWriter, r *http.Request, user *models.User) {
	id, _ := pathID(r)
	stopped := s.Scanner.Cancel(id)
	writeJSON(w, 200, map[string]bool{"ok": true, "stopped": stopped})
}

func (s *Server) handleScanStatus(w http.ResponseWriter, r *http.Request, user *models.User) {
	writeJSON(w, 200, s.Scanner.AllStates())
}

// ---------- items ----------

func (s *Server) handleListItems(w http.ResponseWriter, r *http.Request, user *models.User) {
	q := r.URL.Query()
	libID, _ := strconv.ParseInt(q.Get("library_id"), 10, 64)
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	items, total, err := s.Store.ItemsByLibrary(libID, q.Get("type"), q.Get("search"), q.Get("sort"),
		q.Get("roots") == "1", offset, limit)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if items == nil {
		items = []models.Item{}
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total})
}

func (s *Server) handleItemDetail(w http.ResponseWriter, r *http.Request, user *models.User) {
	id, _ := pathID(r)
	item, err := s.Store.ItemByID(id)
	if err != nil {
		fail(w, 404, "条目不存在")
		return
	}
	lib, _ := s.Store.LibraryByID(item.LibraryID)
	resp := map[string]any{"item": item, "library": lib}

	switch item.Type {
	case "show":
		seasons, _ := s.Store.ItemChildren(item.ID)
		type seasonOut struct {
			models.Item
			Episodes []models.Item `json:"episodes"`
		}
		out := make([]seasonOut, 0, len(seasons))
		for _, se := range seasons {
			eps, _ := s.Store.ItemChildren(se.ID)
			out = append(out, seasonOut{Item: se, Episodes: eps})
		}
		resp["seasons"] = out
	case "season":
		eps, _ := s.Store.ItemChildren(item.ID)
		type episodeOut struct {
			models.Item
			// Versions always holds at least one entry so the season grid can
			// offer "play" for single-release episodes; FileID is the default.
			Versions []versionDTO `json:"versions,omitempty"`
			FileID   int64        `json:"file_id,omitempty"`
		}
		out := make([]episodeOut, 0, len(eps))
		for _, ep := range eps {
			vs := s.versionsForAll(ep.ID, 0)
			var def int64
			if len(vs) > 0 {
				def = vs[0].ID
			}
			out = append(out, episodeOut{Item: ep, Versions: vs, FileID: def})
		}
		resp["episodes"] = out
		if item.ParentID != nil {
			if show, err := s.Store.ItemByID(*item.ParentID); err == nil {
				resp["show"] = show
			}
		}
	default:
		files, _ := s.Store.FilesByItem(item.ID)
		fdtos := make([]map[string]any, 0, len(files))
		for _, f := range files {
			dto := map[string]any{
				"id": f.ID, "size": f.Size, "mtime": f.MTime, "path": f.Path,
				"label": VersionLabel(f.Path),
			}
			if f.Probe != "" {
				if sum := probeSummary(f.Probe); sum != nil {
					dto["media"] = sum
				}
			}
			subs, _ := s.Store.SubtitlesByFile(f.ID)
			dto["subtitles"] = subs
			fdtos = append(fdtos, dto)
		}
		resp["files"] = fdtos
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleItemRefresh(w http.ResponseWriter, r *http.Request, user *models.User) {
	id, _ := pathID(r)
	var req struct {
		TMDBID int `json:"tmdb_id"`
		BGMID  int `json:"bgm_id"`
	}
	_ = readBody(r, &req)
	if req.BGMID > 0 {
		if err := s.Metadata.RefreshItemByBGM(id, req.BGMID); err != nil {
			fail(w, 400, err.Error())
			return
		}
	} else if err := s.Metadata.RefreshItem(id, req.TMDBID); err != nil {
		fail(w, 400, err.Error())
		return
	}
	it, err := s.Store.ItemByID(id)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"item": it})
}

// ---------- home ----------

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request, user *models.User) {
	resp := map[string]any{}

	if progs, err := s.Store.ProgressByUser(user.ID); err == nil {
		type cont struct {
			Item     models.Item `json:"item"`
			FileID   int64       `json:"file_id"`
			Position float64     `json:"position"`
			Duration float64     `json:"duration"`
			Percent  float64     `json:"percent"`
		}
		var list []cont
		for _, p := range progs {
			it, err := s.Store.ItemByID(p.ItemID)
			if err != nil {
				continue
			}
			if p.Duration > 0 && p.Position/p.Duration > 0.95 {
				continue
			}
			pct := 0.0
			if p.Duration > 0 {
				pct = p.Position / p.Duration * 100
			}
			list = append(list, cont{Item: *it, FileID: p.FileID, Position: p.Position, Duration: p.Duration, Percent: pct})
			if len(list) >= 12 {
				break
			}
		}
		resp["continue_watching"] = list
	}

	recent, _ := s.Store.RecentItems("movie", 18)
	if recent == nil {
		recent = []models.Item{}
	}
	resp["recent_movies"] = recent
	recentShows, _ := s.Store.RecentItems("show", 18)
	if recentShows == nil {
		recentShows = []models.Item{}
	}
	resp["recent_shows"] = recentShows
	if n, err := s.Store.CountItems("movie"); err == nil {
		resp["movie_total"] = n
	}
	if n, err := s.Store.CountItems("show"); err == nil {
		resp["show_total"] = n
	}
	libs, _ := s.Store.Libraries()
	resp["libraries"] = libs
	writeJSON(w, 200, resp)
}

// ---------- images ----------

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	kind := r.PathValue("kind")
	item, err := s.Store.ItemByID(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	name := ""
	switch kind {
	case "backdrop":
		name = item.BackdropPath
	case "still":
		name = item.StillPath
	default:
		name = item.PosterPath
	}
	if name == "" {
		s.servePlaceholder(w, r, item.Name)
		return
	}
	full := filepath.Join(s.Cfg.ImagesDir(), filepath.Base(name))
	if _, err := os.Stat(full); err != nil {
		s.servePlaceholder(w, r, item.Name)
		return
	}
	http.ServeFile(w, r, full)
}

func (s *Server) servePlaceholder(w http.ResponseWriter, r *http.Request, title string) {
	t := strings.ReplaceAll(title, "<", "")
	t = strings.ReplaceAll(t, ">", "")
	if rs := []rune(t); len(rs) > 12 {
		t = string(rs[:12])
	}
	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="300" height="450" viewBox="0 0 300 450">
<rect width="300" height="450" fill="#141516"/>
<rect width="300" height="450" fill="url(#g)"/>
<defs><linearGradient id="g" x1="0" y1="0" x2="0" y2="1">
<stop offset="0" stop-color="#1c1c1f"/><stop offset="1" stop-color="#08090a"/>
</linearGradient></defs>
<text x="150" y="220" fill="#62666d" font-size="16" text-anchor="middle" font-family="sans-serif">%s</text>
<text x="150" y="248" fill="#3e3e44" font-size="11" text-anchor="middle" font-family="sans-serif">暂无海报</text>
</svg>`, t)
	w.Header().Set("Content-Type", "image/svg+xml")
	_, _ = w.Write([]byte(svg))
}

// ---------- progress ----------

func (s *Server) handleSaveProgress(w http.ResponseWriter, r *http.Request, user *models.User) {
	var req struct {
		FileID   int64   `json:"file_id"`
		Position float64 `json:"position"`
		Duration float64 `json:"duration"`
	}
	if err := readBody(r, &req); err != nil || req.FileID == 0 {
		fail(w, 400, "file_id 必填")
		return
	}
	if req.Duration <= 0 || req.Position < 0 || req.Position > req.Duration {
		fail(w, 400, "播放进度超出有效范围")
		return
	}
	f, err := s.Store.FileByID(req.FileID)
	if err != nil {
		fail(w, 404, "文件不存在")
		return
	}
	watched := req.Position >= req.Duration*0.9
	p := &models.Progress{UserID: user.ID, FileID: f.ID, ItemID: f.ItemID, Position: req.Position, Duration: req.Duration}
	if err := s.Store.SaveProgress(p); err != nil {
		fail(w, 500, err.Error())
		return
	}
	// Reaching the end of an episode is the only event that writes to Bangumi.
	// The outbox makes repeated progress saves harmless, and checking again on
	// every >=90% save also recovers a local enqueue that failed earlier.
	if s.BangumiSync != nil && watched {
		// EnqueueWatched only touches the local database now. A failure here
		// must not turn a successfully saved playback position into an HTTP
		// error, but it should be visible in the server log for diagnosis.
		if err := s.BangumiSync.EnqueueWatched(user.ID, f.ID); err != nil {
			log.Printf("Bangumi 同步入队失败(user=%d file=%d): %v", user.ID, f.ID, err)
		}
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleListProgress(w http.ResponseWriter, r *http.Request, user *models.User) {
	fileIDStr := r.URL.Query().Get("file_id")
	if fileIDStr != "" {
		fid, _ := strconv.ParseInt(fileIDStr, 10, 64)
		p, err := s.Store.ProgressForFile(user.ID, fid)
		if err != nil {
			writeJSON(w, 200, map[string]any{"position": 0, "duration": 0})
			return
		}
		writeJSON(w, 200, p)
		return
	}
	progs, err := s.Store.ProgressByUser(user.ID)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if progs == nil {
		progs = []models.Progress{}
	}
	writeJSON(w, 200, progs)
}
