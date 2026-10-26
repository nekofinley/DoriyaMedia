package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"doriyamedia/internal/config"
	"doriyamedia/internal/db"
	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

type Manager struct {
	Store *db.Store
	Cfg   *config.Config

	listMu    sync.Mutex
	listCache map[string][]source.Entry // per-EnrichLibrary-run dir listing cache
	// One driver per source. A library may scan several sources, so a file's
	// driver is looked up from the file, not from the library.
	drvMu   sync.Mutex
	drivers map[int64]source.Driver

	bgmMu       sync.Mutex
	bgmEpsCache map[int][]BgmEpisodeMeta
}

func NewManager(store *db.Store, cfg *config.Config) *Manager {
	return &Manager{Store: store, Cfg: cfg}
}

// cachedList returns the directory listing, deduplicating repeated lookups
// within one enrichment run (hundreds of episodes often share a few dirs —
// each remote List costs a public round-trip).
func (m *Manager) cachedList(ctx context.Context, drv source.Driver, sourceID int64, dir string) ([]source.Entry, error) {
	// Keyed by source as well as directory: two sources in one library can hold
	// the same directory path, and a listing from the wrong one would have nfo
	// and artwork looked for in a place the file is not.
	key := fmt.Sprintf("%d|%s", sourceID, dir)
	m.listMu.Lock()
	defer m.listMu.Unlock()
	if m.listCache == nil {
		m.listCache = map[string][]source.Entry{}
	}
	if ents, ok := m.listCache[key]; ok {
		return ents, nil
	}
	if drv == nil {
		return nil, fmt.Errorf("没有可用的存储源驱动(source %d)", sourceID)
	}
	ents, err := drv.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	m.listCache[key] = ents
	return ents, nil
}

// driverFor returns the driver of the source a file lives on. A library may scan
// several sources, so the library's own source_id is not the answer for a file —
// the file's row is. Drivers are kept for the run.
func (m *Manager) driverFor(sourceID int64) source.Driver {
	if sourceID == 0 {
		return nil
	}
	m.drvMu.Lock()
	defer m.drvMu.Unlock()
	if m.drivers == nil {
		m.drivers = map[int64]source.Driver{}
	}
	if d, ok := m.drivers[sourceID]; ok {
		return d
	}
	src, err := m.Store.SourceByID(sourceID)
	if err != nil {
		return nil
	}
	d, err := source.NewDriver(src)
	if err != nil {
		return nil
	}
	m.drivers[sourceID] = d
	return d
}

// httpClient returns the client used for TMDB API/image traffic, routed
// through the configured local proxy when enabled (falls back to a direct
// client if the proxy address is unusable, so scraping never dies silently).
func (m *Manager) httpClient(timeout time.Duration) *http.Client {
	c := m.Cfg.TMDB
	if !c.UseProxy {
		client, _ := HTTPClientFor("", timeout)
		return client
	}
	client, err := HTTPClientFor(c.Proxy, timeout)
	if err != nil {
		log.Printf("TMDB 代理配置无效(%v),本次直连", err)
		client, _ = HTTPClientFor("", timeout)
	}
	return client
}

func (m *Manager) tmdb() *TMDBClient {
	c := m.Cfg.TMDB
	return NewTMDBClient(c.APIKey, c.Language, c.APIBase, c.ImgBase, m.httpClient(30*time.Second))
}

// bgmHTTPClient routes Bangumi traffic through its own proxy setting (api.bgm.tv
// is DNS-poisoned for many mainland users), falling back to a direct client.
func (m *Manager) bgmHTTPClient(timeout time.Duration) *http.Client {
	c := m.Cfg.BGM
	if !c.UseProxy {
		client, _ := HTTPClientFor("", timeout)
		return client
	}
	client, err := HTTPClientFor(c.Proxy, timeout)
	if err != nil {
		log.Printf("Bangumi 代理配置无效(%v),本次直连", err)
		client, _ = HTTPClientFor("", timeout)
	}
	return client
}

func (m *Manager) bgm() *BangumiClient {
	c := m.Cfg.BGM
	return NewBangumiClient(c.Token, c.APIBase, m.bgmHTTPClient(30*time.Second))
}

// BangumiEpisodes exposes the cached subject episode list to features outside
// metadata enrichment (notably per-user watch-progress sync).
func (m *Manager) BangumiEpisodes(ctx context.Context, subjectID int) ([]BgmEpisodeMeta, error) {
	return m.bgmEpisodes(ctx, subjectID)
}

// bgmEpisodes caches one subject's episode list for the duration of an
// enrichment run — otherwise every episode of a show refetches it.
func (m *Manager) bgmEpisodes(ctx context.Context, subjectID int) ([]BgmEpisodeMeta, error) {
	m.bgmMu.Lock()
	if m.bgmEpsCache == nil {
		m.bgmEpsCache = map[int][]BgmEpisodeMeta{}
	}
	if eps, ok := m.bgmEpsCache[subjectID]; ok {
		m.bgmMu.Unlock()
		return eps, nil
	}
	m.bgmMu.Unlock()
	return m.RefreshBangumiEpisodes(ctx, subjectID)
}

// RefreshBangumiEpisodes bypasses the in-process cache. An airing show gains
// episodes after the initial scan, so progress sync needs a way to discover a
// newly aired episode instead of accepting a stale list as final.
func (m *Manager) RefreshBangumiEpisodes(ctx context.Context, subjectID int) ([]BgmEpisodeMeta, error) {
	eps, err := m.bgm().Episodes(ctx, subjectID)
	if err != nil {
		return nil, err
	}
	m.bgmMu.Lock()
	m.bgmEpsCache[subjectID] = eps
	m.bgmMu.Unlock()
	return eps, nil
}

// preferCN returns the Chinese title when available (DoriyaMedia's UI is Chinese),
// otherwise the original one.
func preferCN(cn, orig string) string {
	if strings.TrimSpace(cn) != "" {
		return strings.TrimSpace(cn)
	}
	return strings.TrimSpace(orig)
}

func providers(lib *models.Library) []string {
	var out []string
	if lib.Providers != "" {
		_ = json.Unmarshal([]byte(lib.Providers), &out)
	}
	if len(out) == 0 {
		out = []string{"nfo", "bgm", "tmdb"}
	}
	return out
}

// ---------- rate limit ----------

var (
	rateMu   sync.Mutex
	lastCall time.Time
)

func throttle() {
	rateMu.Lock()
	defer rateMu.Unlock()
	if d := time.Since(lastCall); d < 200*time.Millisecond {
		time.Sleep(200*time.Millisecond - d)
	}
	lastCall = time.Now()
}

// ---------- public API ----------

// SeedListings preloads directory listings gathered during the scan walk so
// the identify pass does not re-fetch them over the network.
func (m *Manager) SeedListings(listings map[string][]source.Entry) {
	m.listMu.Lock()
	defer m.listMu.Unlock()
	if len(listings) == 0 {
		return
	}
	if m.listCache == nil {
		m.listCache = map[string][]source.Entry{}
	}
	for k, v := range listings {
		if _, ok := m.listCache[k]; !ok {
			m.listCache[k] = v
		}
	}
}

// EnrichLibrary identifies all items in a library that lack metadata.
// report is optional progress feedback: report(done, total, human message).
func (m *Manager) EnrichLibrary(lib *models.Library, report func(done, total int, msg string)) {
	say := func(done, total int, msg string) {
		if report != nil {
			report(done, total, msg)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	src, err := m.Store.SourceByID(lib.SourceID)
	if err != nil {
		return
	}
	drv, err := source.NewDriver(src)
	if err != nil {
		return
	}
	defer func() { m.listMu.Lock(); m.listCache = nil; m.listMu.Unlock() }()

	switch lib.Type {
	case "series":
		m.enrichSeries(ctx, drv, lib, say)
	default:
		m.enrichMovies(ctx, drv, lib, say)
	}
}

func (m *Manager) enrichMovies(ctx context.Context, drv source.Driver, lib *models.Library, say func(done, total int, msg string)) {
	items, _, err := m.Store.ItemsByLibrary(lib.ID, "movie", "", "", false, 0, 500000)
	if err != nil {
		return
	}
	for i := range items {
		select {
		case <-ctx.Done():
			return
		default:
		}
		say(i, len(items), "识别 "+items[i].Name)
		m.enrichMovie(ctx, drv, lib, &items[i])
		say(i+1, len(items), "识别 "+items[i].Name)
	}
}

func (m *Manager) enrichSeries(ctx context.Context, drv source.Driver, lib *models.Library, say func(done, total int, msg string)) {
	shows, _, _ := m.Store.ItemsByLibrary(lib.ID, "show", "", "", false, 0, 100000)
	seasons, _, _ := m.Store.ItemsByLibrary(lib.ID, "season", "", "", false, 0, 200000)
	// A series library also holds the show's films ("[show][S0]", a movie sitting
	// beside the shows). They are movie items of this library and are scraped the
	// way any other film is: a film is a film, whatever the library is called.
	films, _, _ := m.Store.ItemsByLibrary(lib.ID, "movie", "", "", false, 0, 100000)
	// count all episodes for the progress denominator
	epTotal := 0
	if all, _, err := m.Store.ItemsByLibrary(lib.ID, "episode", "", "", false, 0, 500000); err == nil {
		epTotal = len(all)
	}
	total := len(shows) + epTotal + len(films)
	done := 0
	for i := range shows {
		say(done, total, "识别剧集 "+shows[i].Name)
		m.enrichShow(ctx, drv, lib, &shows[i])
		done++
		say(done, total, "识别剧集 "+shows[i].Name)
	}
	for i := range films {
		say(done, total, "识别电影 "+films[i].Name)
		m.enrichMovie(ctx, drv, lib, &films[i])
		done++
		say(done, total, "识别电影 "+films[i].Name)
	}
	// episodes after shows are identified
	showByID := map[int64]models.Item{}
	for _, s := range shows {
		showByID[s.ID] = s
	}
	for _, s := range seasons {
		if s.ParentID == nil {
			continue
		}
		show, ok := showByID[*s.ParentID]
		if !ok {
			continue
		}
		kids, _ := m.Store.ItemChildren(s.ID)
		for i := range kids {
			if done%25 == 0 {
				say(done, total, "识别剧集 "+show.Name)
			}
			m.enrichEpisode(ctx, drv, lib, &kids[i], &show, &s)
			done++
		}
	}
	say(total, total, "识别完成")
}

func (m *Manager) enrichMovie(ctx context.Context, drv source.Driver, lib *models.Library, it *models.Item) {
	if it.TMDBID != 0 && it.PosterPath != "" && it.Overview != "" {
		return
	}
	files, err := m.Store.FilesByItem(it.ID)
	if err != nil || len(files) == 0 {
		return
	}
	for _, p := range providers(lib) {
		switch p {
		case "nfo":
			if m.tryMovieNFO(ctx, drv, it, &files[0]) {
				return
			}
		case "tmdb":
			if m.tryMovieTMDB(ctx, it, it.Name, it.Year) {
				return
			}
		case "bgm":
			if m.tryMovieBGM(ctx, it, it.Name, it.Year) {
				return
			}
		}
	}
}

func (m *Manager) enrichShow(ctx context.Context, drv source.Driver, lib *models.Library, show *models.Item) {
	if show.TMDBID != 0 && show.PosterPath != "" && show.Overview != "" {
		return
	}
	filePath, epSource, ok := m.firstEpisodeFile(show.ID)
	if !ok {
		return
	}
	// The show's own files name the source to read its nfo and artwork from.
	if d := m.driverFor(epSource); d != nil {
		drv = d
	}
	for _, p := range providers(lib) {
		switch p {
		case "nfo":
			if m.tryShowNFO(ctx, drv, show, filePath, epSource) {
				return
			}
		case "tmdb":
			if m.tryShowTMDB(ctx, show, show.Name, 0) {
				return
			}
		case "bgm":
			if m.tryShowBGM(ctx, show, show.Name, show.Year) {
				return
			}
		}
	}
}

func (m *Manager) enrichEpisode(ctx context.Context, drv source.Driver, lib *models.Library, ep *models.Item, show *models.Item, season *models.Item) {
	// A name the scan took from the source itself is not a guess to be improved
	// on: bilibili states every episode's title in its listing, and that title is
	// right for *this* season. Asking a provider by episode number is what gave a
	// second season the first season's titles, so a real name is kept and the
	// providers are left to do what they are actually better at — stills, air
	// dates, overview.
	keepName := ""
	if !isPlaceholderEpisodeName(ep) {
		keepName = ep.Name
	}
	if keepName != "" && ep.StillPath != "" {
		return
	}
	defer func() {
		if keepName != "" && ep.Name != keepName {
			ep.Name = keepName
			_ = m.Store.UpdateItemMeta(ep)
		}
	}()

	files, err := m.Store.FilesByItem(ep.ID)
	if err != nil || len(files) == 0 {
		return
	}
	// provider order decides who wins: nfo (precise, local) → Bangumi (Chinese
	// titles for anime) → TMDB season data.
	for _, p := range providers(lib) {
		switch p {
		case "nfo":
			if m.tryEpisodeNFO(ctx, drv, ep, &files[0]) {
				return
			}
		case "bgm":
			if m.tryEpisodeBGM(ctx, ep, show) {
				return
			}
		}
	}
	if show.TMDBID > 0 && m.tmdb().Enabled() {
		throttle()
		sd, err := m.tmdb().TVSeason(ctx, show.TMDBID, season.Season)
		if err == nil && sd != nil {
			for _, e := range sd.Episodes {
				if e.EpisodeNumber == ep.Episode {
					ep.Name = orDefaultStr(e.Name, ep.Name)
					ep.Overview = e.Overview
					ep.AirDate = e.AirDate
					if e.StillPath != "" {
						ep.StillPath = m.saveTMDBImage(ctx, ep.ID, "still", show.TMDBID, e.StillPath, "w300")
					}
					_ = m.Store.UpdateItemMeta(ep)
					return
				}
			}
		}
	}
}

// isPlaceholderEpisodeName reports whether an episode's name is still the one the
// scan writes when the source gave no title of its own — "第 3 集", "Episode 3",
// or nothing. Anything else was named by a source, by an nfo, or by an earlier
// enrichment run, and is worth keeping.
func isPlaceholderEpisodeName(ep *models.Item) bool {
	name := strings.TrimSpace(ep.Name)
	if name == "" {
		return true
	}
	if name == fmt.Sprintf("第 %d 集", ep.Episode) {
		return true
	}
	return strings.HasPrefix(name, "Episode ")
}

// RefreshItem re-identifies one item, optionally pinning a TMDB id.
func (m *Manager) RefreshItem(itemID int64, tmdbID int) error {
	it, err := m.Store.ItemByID(itemID)
	if err != nil {
		return err
	}
	lib, err := m.Store.LibraryByID(it.LibraryID)
	if err != nil {
		return err
	}
	src, err := m.Store.SourceByID(lib.SourceID)
	if err != nil {
		return err
	}
	drv, err := source.NewDriver(src)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	switch it.Type {
	case "movie":
		if tmdbID > 0 {
			return m.tryMovieTMDBByID(ctx, it, tmdbID)
		}
		it.TMDBID, it.BGMID, it.Overview, it.PosterPath, it.BackdropPath = 0, 0, "", "", ""
		_ = m.Store.UpdateItemMeta(it)
		f := firstFile(m.Store, it.ID)
		for _, p := range providers(lib) {
			switch p {
			case "nfo":
				if m.tryMovieNFO(ctx, drv, it, f) {
					return nil
				}
			case "bgm":
				if m.tryMovieBGM(ctx, it, it.Name, it.Year) {
					return nil
				}
			case "tmdb":
				if m.tryMovieTMDB(ctx, it, it.Name, it.Year) {
					return nil
				}
			}
		}
		return nil
	case "show":
		if tmdbID > 0 {
			return m.tryShowTMDBByID(ctx, it, tmdbID)
		}
		it.TMDBID, it.BGMID, it.Overview, it.PosterPath, it.BackdropPath = 0, 0, "", "", ""
		_ = m.Store.UpdateItemMeta(it)
		filePath, epSource, ok := m.firstEpisodeFile(it.ID)
		if !ok {
			return fmt.Errorf("剧集下没有可用的剧集文件")
		}
		if d := m.driverFor(epSource); d != nil {
			drv = d
		}
		for _, p := range providers(lib) {
			switch p {
			case "nfo":
				if m.tryShowNFO(ctx, drv, it, filePath, epSource) {
					return nil
				}
			case "bgm":
				if m.tryShowBGM(ctx, it, it.Name, it.Year) {
					return nil
				}
			case "tmdb":
				if m.tryShowTMDB(ctx, it, it.Name, 0) {
					return nil
				}
			}
		}
		return nil
	default:
		return fmt.Errorf("该类型不支持手动识别")
	}
}

// RefreshItemByBGM pins an item to a specific Bangumi subject (manual identify
// for anime, where search results are often ambiguous).
func (m *Manager) RefreshItemByBGM(itemID int64, bgmID int) error {
	it, err := m.Store.ItemByID(itemID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	sub, err := m.bgm().Subject(ctx, bgmID)
	if err != nil {
		return err
	}
	return m.applyBGM(ctx, it, sub)
}

func firstFile(store *db.Store, itemID int64) *models.MediaFile {
	fls, _ := store.FilesByItem(itemID)
	if len(fls) == 0 {
		return nil
	}
	return &fls[0]
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func orDefaultStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// ---------- nfo ----------

func (m *Manager) tryMovieNFO(ctx context.Context, drv source.Driver, it *models.Item, f *models.MediaFile) bool {
	if f == nil || isHTTP(f.Path) {
		return false
	}
	// The file's own source decides where to look: a library may scan several.
	if d := m.driverFor(f.SourceID); d != nil {
		drv = d
	}
	dir, base := dirBase(f.Path)
	stem := fileStem(base)
	list, err := m.cachedList(ctx, drv, f.SourceID, dir)
	if err != nil {
		return false
	}
	names := map[string]source.Entry{}
	for _, e := range list {
		names[strings.ToLower(e.Name)] = e
	}
	var nfoPath string
	for _, cand := range []string{stem + ".nfo", base + ".nfo", "movie.nfo", baseName2(dir) + ".nfo"} {
		if e, ok := names[strings.ToLower(cand)]; ok && !e.IsDir {
			nfoPath = e.Path
			break
		}
	}
	if nfoPath == "" {
		return false
	}
	raw, err := readSmall(ctx, drv.Open, nfoPath)
	if err != nil {
		return false
	}
	nfo, kind, err := ParseNFO(raw)
	if err != nil || (kind != "movie" && kind != "tvshow") {
		return false
	}
	apply := nfo.Title != ""
	if !apply {
		return false
	}
	if nfo.Title != "" {
		it.Name = nfo.Title
	}
	if nfo.OriginalTitle != "" {
		it.OriginalTitle = nfo.OriginalTitle
	}
	if nfo.Year > 0 {
		it.Year = nfo.Year
	}
	if nfo.Plot != "" {
		it.Overview = nfo.Plot
	}
	if nfo.Rating > 0 {
		it.Rating = nfo.Rating
	}
	if id := nfo.TMDBID(); id > 0 {
		it.TMDBID = id
	}
	// local artwork files
	if p := findImage(names, []string{"poster.jpg", "folder.jpg", "cover.jpg",
		stem + "-poster.jpg", stem + "-thumb.jpg", base + "-poster.jpg", base + "-thumb.jpg"}); p != "" {
		it.PosterPath = m.saveRawImage(ctx, drv, p, it.ID, "poster")
	}
	if p := findImage(names, []string{"fanart.jpg", "backdrop.jpg", stem + "-fanart.jpg", base + "-fanart.jpg"}); p != "" {
		it.BackdropPath = m.saveRawImage(ctx, drv, p, it.ID, "backdrop")
	}
	// artwork URLs inside nfo
	if it.PosterPath == "" {
		if u := nfo.PosterURL(); u != "" {
			it.PosterPath = m.saveURLImage(ctx, u, it.ID, "poster")
		}
	}
	if it.BackdropPath == "" {
		if u := nfo.FanartURL(); u != "" {
			it.BackdropPath = m.saveURLImage(ctx, u, it.ID, "backdrop")
		}
	}
	return m.Store.UpdateItemMeta(it) == nil
}

func (m *Manager) tryEpisodeNFO(ctx context.Context, drv source.Driver, ep *models.Item, f *models.MediaFile) bool {
	if f == nil || isHTTP(f.Path) {
		return false
	}
	if d := m.driverFor(f.SourceID); d != nil {
		drv = d
	}
	dir, file := dirBase(f.Path)
	stem := fileStem(file)
	list, err := m.cachedList(ctx, drv, f.SourceID, dir)
	if err != nil {
		return false
	}
	names := map[string]source.Entry{}
	for _, e := range list {
		names[strings.ToLower(e.Name)] = e
	}
	// Kodi names sidecar files after the video without its extension
	// (Show S01E01.mkv → Show S01E01.nfo); keep the old exact-name form as a
	// fallback so existing libraries keep working.
	var nfoPath string
	for _, cand := range []string{stem + ".nfo", file + ".nfo", "episode.nfo"} {
		if e, ok := names[strings.ToLower(cand)]; ok && !e.IsDir {
			nfoPath = e.Path
			break
		}
	}
	if nfoPath == "" {
		return false
	}
	raw, err := readSmall(ctx, drv.Open, nfoPath)
	if err != nil {
		return false
	}
	nfo, kind, err := ParseNFO(raw)
	if err != nil || kind != "episode" {
		return false
	}
	if nfo.Title != "" {
		ep.Name = nfo.Title
	}
	if nfo.Plot != "" {
		ep.Overview = nfo.Plot
	}
	if nfo.Season > 0 {
		ep.Season = nfo.Season
	}
	if nfo.Episode > 0 {
		ep.Episode = nfo.Episode
	}
	if nfo.Aired != "" {
		ep.AirDate = nfo.Aired
	}
	if p := findImage(names, []string{stem + "-thumb.jpg", "thumb.jpg", file + "-thumb.jpg"}); p != "" {
		ep.StillPath = m.saveRawImage(ctx, drv, p, ep.ID, "still")
	}
	return m.Store.UpdateItemMeta(ep) == nil
}

func (m *Manager) tryShowNFO(ctx context.Context, drv source.Driver, show *models.Item, filePath string, sourceID int64) bool {
	if isHTTP(filePath) {
		return false
	}
	dir, _ := dirBase(filePath)
	for depth := 0; depth < 3 && dir != "" && dir != "/" && dir != "."; depth++ {
		list, err := m.cachedList(ctx, drv, sourceID, dir)
		if err != nil {
			return false
		}
		for _, e := range list {
			if !e.IsDir && strings.EqualFold(e.Name, "tvshow.nfo") {
				raw, err := readSmall(ctx, drv.Open, e.Path)
				if err != nil {
					return false
				}
				nfo, kind, err := ParseNFO(raw)
				if err != nil || (kind != "tvshow" && kind != "movie") {
					return false
				}
				if nfo.Title != "" {
					show.Name = nfo.Title
				}
				if nfo.OriginalTitle != "" {
					show.OriginalTitle = nfo.OriginalTitle
				}
				if nfo.Year > 0 {
					show.Year = nfo.Year
				}
				if nfo.Plot != "" {
					show.Overview = nfo.Plot
				}
				if nfo.Rating > 0 {
					show.Rating = nfo.Rating
				}
				if id := nfo.TMDBID(); id > 0 {
					show.TMDBID = id
				}
				names := map[string]source.Entry{}
				for _, e2 := range list {
					names[strings.ToLower(e2.Name)] = e2
				}
				if p := findImage(names, []string{"poster.jpg", "folder.jpg", "banner.jpg"}); p != "" {
					show.PosterPath = m.saveRawImage(ctx, drv, p, show.ID, "poster")
				}
				if p := findImage(names, []string{"fanart.jpg", "backdrop.jpg"}); p != "" {
					show.BackdropPath = m.saveRawImage(ctx, drv, p, show.ID, "backdrop")
				}
				if show.PosterPath == "" {
					if u := nfo.PosterURL(); u != "" {
						show.PosterPath = m.saveURLImage(ctx, u, show.ID, "poster")
					}
				}
				return m.Store.UpdateItemMeta(show) == nil
			}
		}
		dir = parentDir(dir)
	}
	return false
}

func findImage(names map[string]source.Entry, candidates []string) string {
	for _, c := range candidates {
		if e, ok := names[strings.ToLower(c)]; ok && !e.IsDir {
			return e.Path
		}
	}
	return ""
}

// ---------- tmdb ----------

// searchTMDB asks for a title in every spelling the providers index. A 港澳台
// source names its shows in Traditional Chinese; TMDB's zh-CN titles are
// Simplified, and a search for the Traditional spelling can return nothing at
// all. The original spelling is tried first, then the Simplified one; a hit
// that resembles the title wins, otherwise the first answer of the first
// non-empty search is kept (the behaviour before this existed).
func (m *Manager) searchTMDB(ctx context.Context, kind, query string, year int) int {
	t := m.tmdb()
	if !t.Enabled() || strings.TrimSpace(query) == "" {
		return 0
	}
	fallback := 0
	for _, spelling := range searchSpellings(query) {
		throttle()
		var results []TMDBSearchItem
		var err error
		if kind == "movie" {
			results, err = t.SearchMovie(ctx, spelling, year)
		} else {
			results, err = t.SearchTV(ctx, spelling, year)
		}
		if err != nil || len(results) == 0 {
			continue
		}
		best, score := pickTMDB(results, query, year)
		if fallback == 0 {
			fallback = results[0].ID
		}
		if score >= 60 {
			return results[best].ID
		}
	}
	return fallback
}

// pickTMDB returns the index and score of the result that best matches the
// title. Only names and the year are used: taking results[0] blindly is what
// turns a Traditional title with no exact hit into a random show.
func pickTMDB(results []TMDBSearchItem, query string, year int) (int, int) {
	nq := normTitle(query)
	best, bestScore := 0, 0
	for i, r := range results {
		score := 10
		for _, name := range []string{r.Title, r.Name, r.OriginalTitle, r.OriginalName} {
			nn := normTitle(name)
			if nn == "" {
				continue
			}
			switch {
			case nn == nq:
				if score < 100 {
					score = 100
				}
			case nearMatch(nn, nq), titleClose(nn, nq):
				if score < 60 {
					score = 60
				}
			}
		}
		if year > 0 {
			date := r.ReleaseDate
			if date == "" {
				date = r.FirstAirDate
			}
			if len(date) >= 4 && atoi(date[:4]) == year {
				score += 30
			}
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	return best, bestScore
}

func (m *Manager) tryMovieTMDB(ctx context.Context, it *models.Item, query string, year int) bool {
	id := m.searchTMDB(ctx, "movie", query, year)
	if id == 0 {
		return false
	}
	return m.tryMovieTMDBByID(ctx, it, id) == nil
}

func (m *Manager) tryMovieTMDBByID(ctx context.Context, it *models.Item, id int) error {
	t := m.tmdb()
	throttle()
	d, err := t.MovieDetail(ctx, id)
	if err != nil {
		return err
	}
	it.TMDBID = d.ID
	if d.Title != "" {
		it.Name = d.Title
	}
	it.OriginalTitle = d.OriginalTitle
	it.Overview = d.Overview
	it.Rating = d.VoteAverage
	if d.ReleaseDate != "" && len(d.ReleaseDate) >= 4 {
		it.Year = atoi(d.ReleaseDate[:4])
	}
	it.Genres = genresJSON(d.Genres)
	if d.PosterPath != "" {
		it.PosterPath = m.saveTMDBImage(ctx, it.ID, "poster", d.ID, d.PosterPath, "w500")
	}
	if d.BackdropPath != "" {
		it.BackdropPath = m.saveTMDBImage(ctx, it.ID, "backdrop", d.ID, d.BackdropPath, "w1280")
	}
	return m.Store.UpdateItemMeta(it)
}

func (m *Manager) tryShowTMDB(ctx context.Context, show *models.Item, query string, year int) bool {
	id := m.searchTMDB(ctx, "tv", query, year)
	if id == 0 {
		return false
	}
	return m.tryShowTMDBByID(ctx, show, id) == nil
}

func (m *Manager) tryShowTMDBByID(ctx context.Context, show *models.Item, id int) error {
	t := m.tmdb()
	throttle()
	d, err := t.TVDetail(ctx, id)
	if err != nil {
		return err
	}
	show.TMDBID = d.ID
	if d.Name != "" {
		show.Name = d.Name
	}
	show.OriginalTitle = d.OriginalName
	show.Overview = d.Overview
	show.Rating = d.VoteAverage
	if d.FirstAirDate != "" && len(d.FirstAirDate) >= 4 {
		show.Year = atoi(d.FirstAirDate[:4])
	}
	show.Genres = genresJSON(d.Genres)
	if d.PosterPath != "" {
		show.PosterPath = m.saveTMDBImage(ctx, show.ID, "poster", d.ID, d.PosterPath, "w500")
	}
	if d.BackdropPath != "" {
		show.BackdropPath = m.saveTMDBImage(ctx, show.ID, "backdrop", d.ID, d.BackdropPath, "w1280")
	}
	return m.Store.UpdateItemMeta(show)
}

// ---------- bangumi ----------

func (m *Manager) tryMovieBGM(ctx context.Context, it *models.Item, query string, year int) bool {
	sub, err := m.searchBGM(ctx, query, year)
	if err != nil || sub == nil {
		return false
	}
	return m.applyBGM(ctx, it, sub) == nil
}

func (m *Manager) tryShowBGM(ctx context.Context, show *models.Item, query string, year int) bool {
	sub, err := m.searchBGM(ctx, query, year)
	if err != nil || sub == nil {
		return false
	}
	return m.applyBGM(ctx, show, sub) == nil
}

// searchBGM finds the best Bangumi subject for a name: exact title match first,
// then a one-character edit, with the year as a tie-breaker.
//
// When nothing in the search results is close enough, it retries with spellings
// that drop one character. Release folder names routinely carry one extra or
// wrong character ("别再当欧尼酱了" vs "别当欧尼酱了！", "监狱学院" vs "监狱学园"),
// and the search engine cannot find the subject from the longer spelling at all.
// Every rewritten search is scored against the *original* name, so a variant
// keyword can never pick a subject that does not resemble the title.
func (m *Manager) searchBGM(ctx context.Context, query string, year int) (*BgmSubject, error) {
	q := strings.TrimSpace(query)
	if q == "" || len([]rune(q)) < 2 {
		return nil, nil
	}
	b := m.bgm()
	if !b.Enabled() {
		return nil, nil
	}
	// A 港澳台 source states its titles in Traditional Chinese, while the
	// providers index the Simplified spelling. Both spellings are searched, the
	// original first; every hit is still scored against the original name
	// (normTitle folds the two forms together), so a variant search cannot
	// select an unrelated subject.
	var variantBases []string
	for _, spelling := range searchSpellings(q) {
		throttle()
		results, err := b.Search(ctx, spelling, BgmTypeAnime)
		if err != nil {
			return nil, err
		}
		if sub := m.bgmSubjectFor(ctx, b, q, year, results); sub != nil {
			m.logApproxBGM(q, sub)
			return sub, nil
		}
		if len(results) > 0 {
			variantBases = append(variantBases, spelling)
		}
	}
	// A title the engine did not understand well enough to return anything is
	// not going to be found by deleting a character either; the retries are for
	// the common case where wrong hits came back instead of the right one.
	// bgmQueryVariants normalizes first, so the Traditional and Simplified bases
	// can produce the same variants; each spelling is asked for once.
	tried := map[string]bool{}
	for _, base := range variantBases {
		for _, variant := range bgmQueryVariants(base) {
			if tried[variant] {
				continue
			}
			tried[variant] = true
			throttle()
			hits, err := b.Search(ctx, variant, BgmTypeAnime)
			if err != nil {
				return nil, err
			}
			if sub := m.bgmSubjectFor(ctx, b, q, year, hits); sub != nil {
				m.logApproxBGM(q, sub)
				return sub, nil
			}
		}
	}
	return nil, nil
}

// bgmSubjectFor picks the best hit for query and loads its detail (rating, tags
// and summary that search results may omit). The detail call is best-effort:
// the search hit is still usable metadata when it fails.
func (m *Manager) bgmSubjectFor(ctx context.Context, b *BangumiClient, query string, year int, results []BgmSubject) *BgmSubject {
	best := pickBGM(query, year, results)
	if best < 0 {
		return nil
	}
	if d, err := b.Subject(ctx, results[best].ID); err == nil && d != nil {
		return d
	}
	return &results[best]
}

// logApproxBGM leaves a trace when a subject was accepted although neither of
// its titles equals the filename, so an occasional wrong pick can be spotted
// and pinned by id from the detail page.
func (m *Manager) logApproxBGM(query string, sub *BgmSubject) {
	if sub == nil {
		return
	}
	nq := normTitle(query)
	if nq == normTitle(sub.NameCN) || nq == normTitle(sub.Name) {
		return
	}
	log.Printf("Bangumi 近似匹配: %q → #%d《%s》", query, sub.ID, preferCN(sub.NameCN, sub.Name))
}

// pickBGM returns the index of the best matching search result, or -1 when
// nothing is close enough. Below 60 means neither the Chinese nor the original
// title really matches — better no metadata than a random anime (a mock or
// fictional name must not become "Another (2012)").
func pickBGM(query string, year int, results []BgmSubject) int {
	nq := normTitle(query)
	best, bestScore := -1, 0
	for i, r := range results {
		score := 20
		ncn, norig := normTitle(r.NameCN), normTitle(r.Name)
		switch {
		case ncn != "" && ncn == nq, norig != "" && norig == nq:
			score = 100
		case nearMatch(ncn, nq), nearMatch(norig, nq),
			titleClose(ncn, nq), titleClose(norig, nq):
			score = 60
		}
		if year > 0 && r.Year == year {
			score += 30
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	if bestScore < 60 {
		return -1
	}
	return best
}

// titleClose reports whether two normalized titles differ by only a character
// or two, which is how release folder names usually diverge from the official
// title ("别再当欧尼酱了" / "别当欧尼酱了", "监狱学院" / "监狱学园").
func titleClose(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) > len(rb) {
		ra, rb = rb, ra
	}
	short, long := len(ra), len(rb)
	if short < 3 {
		return false
	}
	common := lcsLen(ra, rb)
	if long <= 6 {
		return common >= long-1
	}
	return float64(common) >= 0.8*float64(long)
}

// lcsLen returns the length of the longest common subsequence of a and b.
func lcsLen(a, b []rune) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			switch {
			case a[i-1] == b[j-1]:
				cur[j] = prev[j-1] + 1
			case prev[j] >= cur[j-1]:
				cur[j] = prev[j]
			default:
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// bgmQueryVariants returns one-character-deleted spellings of a title, most
// promising first. Characters that release names tend to add are tried first,
// so the usual mismatch ("别再…" for "别当…") costs a single extra request.
// Only Han titles are expanded: dropping a Latin character rarely produces the
// official title, and TMDB is the better fallback for those.
func bgmQueryVariants(title string) []string {
	base := normTitle(title)
	runes := []rune(base)
	if len(runes) < 4 || len(runes) > 24 || !hasHan(base) {
		return nil
	}
	const filler = "再又了的之吧呢啊呀啦嘛哦"
	seen := make([]bool, len(runes))
	order := make([]int, 0, len(runes))
	for _, f := range filler {
		for i, r := range runes {
			if r == f && !seen[i] {
				seen[i] = true
				order = append(order, i)
			}
		}
	}
	for i := range runes {
		if !seen[i] {
			order = append(order, i)
		}
	}
	out := make([]string, 0, len(order))
	used := map[string]bool{}
	for _, i := range order {
		v := string(runes[:i]) + string(runes[i+1:])
		if len([]rune(v)) < 3 || used[v] {
			continue
		}
		used[v] = true
		out = append(out, v)
		if len(out) >= 8 {
			break
		}
	}
	return out
}

func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// nearMatch is substring containment with a length-ratio guard, so a short
// generic title ("Another") cannot swallow a long unrelated one.
func nearMatch(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	short, long := a, b
	if len([]rune(a)) > len([]rune(b)) {
		short, long = b, a
	}
	if len([]rune(short)) < 2 || !strings.Contains(long, short) {
		return false
	}
	return float64(len([]rune(short))) >= 0.6*float64(len([]rune(long)))
}

func (m *Manager) applyBGM(ctx context.Context, it *models.Item, sub *BgmSubject) error {
	it.BGMID = sub.ID
	// Chinese title wins for the UI; the original goes to original_title.
	if name := preferCN(sub.NameCN, sub.Name); name != "" {
		it.Name = name
	}
	if orig := strings.TrimSpace(sub.Name); orig != "" && orig != it.Name {
		it.OriginalTitle = orig
	}
	if sub.Year > 0 {
		it.Year = sub.Year
	}
	if sub.Overview != "" {
		it.Overview = sub.Overview
	}
	if sub.Rating > 0 {
		it.Rating = sub.Rating
	}
	if len(sub.Genres) > 0 {
		raw, _ := json.Marshal(sub.Genres)
		it.Genres = string(raw)
	}
	if sub.PosterURL != "" && it.PosterPath == "" {
		it.PosterPath = m.saveBGMImage(ctx, it.ID, "poster", sub.PosterURL)
	}
	return m.Store.UpdateItemMeta(it)
}

// tryEpisodeBGM fills the episode number's Chinese title and air date from the
// show's Bangumi subject (cached per run, so one fetch serves every episode).
func (m *Manager) tryEpisodeBGM(ctx context.Context, ep *models.Item, show *models.Item) bool {
	if show == nil || show.BGMID == 0 || ep.Episode <= 0 {
		return false
	}
	eps, err := m.bgmEpisodes(ctx, show.BGMID)
	if err != nil || len(eps) == 0 {
		return false
	}
	for _, e := range eps {
		if e.Number != ep.Episode {
			continue
		}
		if t := strings.TrimSpace(e.Title); t != "" {
			ep.Name = t
		}
		if e.AirDate != "" {
			ep.AirDate = e.AirDate
		}
		_ = m.Store.UpdateItemMeta(ep)
		return true
	}
	return false
}

func (m *Manager) saveBGMImage(ctx context.Context, itemID int64, kind, rawURL string) string {
	u := BgmImageProxyURL(rawURL, "l")
	data, err := m.bgm().DownloadImage(ctx, u)
	if err != nil || len(data) < 100 {
		return ""
	}
	return m.writeImage(itemID, kind, 0, u, data)
}

// normTitle lowercases, folds Traditional to Simplified and strips
// punctuation/spaces so "孤独摇滚！", "孤獨搖滾！" and "孤独摇滚" compare equal.
func normTitle(s string) string {
	s = strings.ToLower(strings.TrimSpace(toSimplified(s)))
	var b strings.Builder
	for _, r := range s {
		switch r {
		case ' ', '\t', '　', '!', '！', '?', '？', '.', '。', '·', '・', '、', ',', '，',
			'-', '_', '—', '～', '~', ':', '：', '\'', '"', '「', '」', '《', '》', '(', ')', '（', '）':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ---------- images ----------

func (m *Manager) saveTMDBImage(ctx context.Context, itemID int64, kind string, tmdbID int, path, size string) string {
	t := m.tmdb()
	data, err := t.DownloadImage(ctx, path, size)
	if err != nil || len(data) < 100 {
		return ""
	}
	return m.writeImage(itemID, kind, tmdbID, path, data)
}

func (m *Manager) saveURLImage(ctx context.Context, u string, itemID int64, kind string) string {
	client := m.httpClient(60 * time.Second)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil || len(data) < 100 {
		return ""
	}
	return m.writeImage(itemID, kind, 0, u, data)
}

func (m *Manager) saveRawImage(ctx context.Context, drv source.Driver, p string, itemID int64, kind string) string {
	rc, err := drv.Open(ctx, p)
	if err != nil {
		return ""
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, 20<<20))
	if err != nil || len(data) < 100 {
		return ""
	}
	return m.writeImage(itemID, kind, 0, p, data)
}

func (m *Manager) writeImage(itemID int64, kind string, tmdbID int, srcPath string, data []byte) string {
	dir := m.Cfg.ImagesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	name := fmt.Sprintf("item_%d_%s", itemID, kind)
	if tmdbID > 0 {
		name = fmt.Sprintf("tmdb_%d_%s", tmdbID, kind)
	}
	switch {
	case strings.Contains(strings.ToLower(srcPath), ".png"):
		name += ".png"
	case strings.Contains(strings.ToLower(srcPath), ".webp"):
		name += ".webp"
	default:
		name += ".jpg"
	}
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return ""
	}
	return name
}

// ---------- helpers ----------

func (m *Manager) firstEpisodeFile(showID int64) (string, int64, bool) {
	seasons, _ := m.Store.ItemChildren(showID)
	for _, s := range seasons {
		eps, _ := m.Store.ItemChildren(s.ID)
		for _, e := range eps {
			fls, _ := m.Store.FilesByItem(e.ID)
			if len(fls) > 0 {
				return fls[0].Path, fls[0].SourceID, true
			}
		}
	}
	return "", 0, false
}

func isHTTP(p string) bool {
	return strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://")
}

func dirBase(p string) (string, string) {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

// fileStem drops the final extension of a file name ("a.b.mkv" → "a.b").
func fileStem(name string) string {
	if i := strings.LastIndex(name, "."); i > 0 {
		return name[:i]
	}
	return name
}

func baseName2(dir string) string {
	_, b := dirBase(strings.TrimSuffix(dir, "/"))
	return b
}

func parentDir(dir string) string {
	if i := strings.LastIndex(dir, "/"); i > 0 {
		return dir[:i]
	}
	return ""
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func genresJSON(list []struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}) string {
	names := make([]string, 0, len(list))
	for _, g := range list {
		names = append(names, g.Name)
	}
	raw, _ := json.Marshal(names)
	return string(raw)
}
