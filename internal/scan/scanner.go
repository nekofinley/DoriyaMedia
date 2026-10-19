package scan

import (
	"context"
	"fmt"
	"io"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"doriyamedia/internal/config"
	"doriyamedia/internal/db"
	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

type fileRec struct {
	Path  string // driver path, or absolute URL for .strm
	Dir   string
	Size  int64
	MTime int64
	Strm  bool
	// SourceID and Root are where this file came from. A library may scan more
	// than one source, so neither can be read off the library: the file's own row
	// has to carry them, or two sources would be organised as if they were one
	// tree and the wrong rows would be deleted on the next scan.
	SourceID int64
	Root     string
}

type Scanner struct {
	Store      *db.Store
	Cfg        *config.Config
	OnIdentify func(lib *models.Library, listings map[string][]source.Entry, report func(done, total int, msg string)) // metadata enrichment hook
	// OnScanned fires after a library scan finishes, so the caller can warm
	// caches (e.g. pre-probe new files) without blocking the scan itself.
	OnScanned func(lib *models.Library)
	states    sync.Map // libraryID -> *models.ScanProgress
	cancels   sync.Map // libraryID -> context.CancelFunc

	queue chan *models.Library
	once  sync.Once
}

// Cancel aborts a running scan of the given library (best effort).
func (sc *Scanner) Cancel(libID int64) bool {
	if v, ok := sc.cancels.Load(libID); ok {
		v.(context.CancelFunc)()
		return true
	}
	return false
}

// State reports a scan's progress. The result is a copy: the scan keeps running
// while the caller marshals it.
func (sc *Scanner) State(libID int64) models.ScanState {
	if v, ok := sc.states.Load(libID); ok {
		st, _ := v.(*models.ScanProgress).Snapshot(0)
		return st
	}
	return models.ScanState{LibraryID: libID, Phase: "idle"}
}

// AllStates snapshots every tracked scan, for the settings page poll.
func (sc *Scanner) AllStates() []models.ScanState {
	var out []models.ScanState
	sc.states.Range(func(_, v any) bool {
		st, _ := v.(*models.ScanProgress).Snapshot(0)
		out = append(out, st)
		return true
	})
	return out
}

func (sc *Scanner) setState(libID int64, p *models.ScanProgress) {
	sc.states.Store(libID, p)
}

// ScanLibrary enqueues a library scan (processed sequentially by one worker).
func (sc *Scanner) ScanLibrary(lib *models.Library) error {
	sc.once.Do(func() {
		sc.queue = make(chan *models.Library, 64)
		go sc.worker()
	})
	select {
	case sc.queue <- lib:
		return nil
	default:
		return fmt.Errorf("扫描队列已满,请稍后再试")
	}
}

func (sc *Scanner) worker() {
	for lib := range sc.queue {
		sc.runScan(lib)
	}
}

func (sc *Scanner) runScan(lib *models.Library) error {
	prog := models.NewScanProgress(lib.ID)
	prog.Set(models.ScanState{
		LibraryID: lib.ID, Running: true, Phase: "walk",
		StartedAt: float64(time.Now().UnixMilli()) / 1000,
	})
	prog.AddEvent("info", "开始扫描 "+lib.Name)
	sc.setState(lib.ID, prog)

	fail := func(msg string) {
		prog.Update(func(st *models.ScanState) {
			st.Running, st.Error, st.Phase = false, msg, "error"
			st.FinishedAt = float64(time.Now().UnixMilli()) / 1000
		})
		prog.AddEvent("warn", msg)
	}

	// Where this library scans. A library may hold several places; each is walked
	// with its own driver, and every file found keeps the source it came from.
	places, err := sc.Store.LibrarySources(lib.ID)
	if err != nil || len(places) == 0 {
		places = []models.LibrarySource{{LibraryID: lib.ID, SourceID: lib.SourceID, Path: lib.Path}}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	sc.cancels.Store(lib.ID, context.CancelFunc(cancel))
	defer sc.cancels.Delete(lib.ID)
	finish := func() {
		prog.Update(func(st *models.ScanState) {
			st.Running = false
			st.FinishedAt = float64(time.Now().UnixMilli()) / 1000
		})
	}

	// The walk reports every directory. Emitting one line per directory would
	// bury everything else on a large library, so the feed takes every eighth
	// plus whatever is current — enough to see it moving.
	var walkSeen int
	onDir := func(dir string, dirs, found, errs int64, firstErr string) {
		walkSeen++
		prog.Update(func(st *models.ScanState) {
			st.Current = dir
			st.DirsDone, st.FilesFound, st.WalkErrs = int(dirs), int(found), int(errs)
			if firstErr != "" && st.FirstErr == "" {
				st.FirstErr = firstErr
			}
		})
		if walkSeen%8 == 0 {
			prog.AddEvent("dir", dir)
		}
	}

	var files []fileRec
	listings := map[string][]source.Entry{}
	// walked records the sources this scan read to the end. Cleanup only touches
	// those: a source that could not be listed is not a source whose files are
	// gone, and deleting them would throw away the library rows and the watch
	// progress attached to them.
	walked := map[int64]bool{}
	current := map[int64]bool{}
	var srcErrs []string
	for _, place := range places {
		current[place.SourceID] = true
		if ctx.Err() != nil {
			break
		}
		src, serr := sc.Store.SourceByID(place.SourceID)
		if serr != nil {
			srcErrs = append(srcErrs, fmt.Sprintf("源 %d: %v", place.SourceID, serr))
			continue
		}
		drv, derr := source.NewDriver(src)
		if derr != nil {
			srcErrs = append(srcErrs, fmt.Sprintf("%s: %v", src.Name, derr))
			continue
		}
		prog.AddEvent("info", fmt.Sprintf("扫描 %s %s", src.Name, place.Path))
		f, l, werr := walkTree(ctx, drv, place.Path, sc.Cfg.ScanConcurrency, place.SourceID, onDir)
		for k, v := range l {
			listings[k] = v
		}
		files = append(files, f...)
		if werr == nil {
			walked[place.SourceID] = true
		} else {
			srcErrs = append(srcErrs, fmt.Sprintf("%s: %v", src.Name, werr))
		}
	}
	if ctx.Err() != nil {
		prog.Update(func(st *models.ScanState) { st.Phase = "error" })
		fail("扫描已取消")
		finish()
		return ctx.Err()
	}
	if len(files) == 0 && len(srcErrs) > 0 {
		fail("遍历失败: " + strings.Join(srcErrs, "; "))
		finish()
		return fmt.Errorf("scan: %s", strings.Join(srcErrs, "; "))
	}
	if len(srcErrs) > 0 {
		prog.AddEvent("warn", "部分源读取失败(不清理这些源): "+strings.Join(srcErrs, "; "))
	}

	prog.Update(func(st *models.ScanState) {
		st.Phase = "organize"
		st.Total = len(files)
		st.Done = 0
		st.Current = fmt.Sprintf("发现 %d 个视频文件,正在整理…", len(files))
	})
	prog.AddEvent("file", fmt.Sprintf("发现 %d 个视频文件", len(files)))
	if err := sc.organize(lib, files, prog, current, walked); err != nil {
		fail("整理失败: " + err.Error())
		finish()
		return err
	}

	prog.Update(func(st *models.ScanState) {
		st.Phase = "identify"
		st.Done, st.Total = 0, 0
		st.Current = "刮削与识别…"
	})
	prog.AddEvent("meta", "开始刮削与识别")
	if sc.OnIdentify != nil {
		sc.OnIdentify(lib, listings, func(done, total int, msg string) {
			prog.Update(func(st *models.ScanState) {
				st.Done, st.Total, st.Current = done, total, msg
			})
			if msg != "" {
				prog.AddEvent("meta", msg)
			}
		})
	}
	// Settle the summary from the finished counters, then report it.
	final, _ := prog.Snapshot(0)
	prog.Update(func(st *models.ScanState) {
		st.Phase = "done"
		if final.WalkErrs > 0 || final.FirstErr != "" {
			st.Error = fmt.Sprintf("完成,但有 %d 个目录读取失败(%s)", final.WalkErrs, final.FirstErr)
		} else {
			st.Current = fmt.Sprintf("完成,共 %d 个视频文件", st.Total)
		}
	})
	prog.AddEvent("meta", fmt.Sprintf("扫描完成:%d 个视频文件", final.Total))
	finish()
	if sc.OnScanned != nil {
		sc.OnScanned(lib)
	}
	return nil
}

const maxDepth = 8

func walkTree(ctx context.Context, drv source.Driver, root string, concurrency int, srcID int64, onDir func(dir string, dirs, files, errs int64, firstErr string)) ([]fileRec, map[string][]source.Entry, error) {
	if concurrency <= 0 {
		concurrency = 4
	}
	// Listings are shared with the identify pass so it does not re-list every
	// directory over the network. They are keyed per source: two sources in one
	// library can hold the same directory path, and a listing from the wrong one
	// would have artwork and nfo looked up in a place the file is not.
	dirKey := func(dir string) string { return fmt.Sprintf("%d|%s", srcID, dir) }
	sem := make(chan struct{}, concurrency)
	var mu sync.Mutex
	var files []fileRec
	var firstErr error
	var dirsDone, filesFound, errCount atomic.Int64
	var wg sync.WaitGroup
	listings := map[string][]source.Entry{} // shared with identify to avoid re-listing

	report := func(dir string) {
		if onDir == nil {
			return
		}
		fe := ""
		mu.Lock()
		if firstErr != nil {
			fe = firstErr.Error()
		}
		mu.Unlock()
		onDir(dir, dirsDone.Load(), filesFound.Load(), errCount.Load(), fe)
	}

	var walkDir func(dir string, depth int)
	walkDir = func(dir string, depth int) {
		defer wg.Done()
		if depth > maxDepth {
			return
		}
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
			defer func() { <-sem }()
		}
		dirsDone.Add(1)
		report(dir)
		entries, err := drv.List(ctx, dir)
		if err != nil {
			mu.Lock()
			if firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
			errCount.Add(1)
			return
		}
		mu.Lock()
		listings[dirKey(dir)] = entries
		mu.Unlock()
		for _, e := range entries {
			if SkipEntry(e.Name) {
				continue
			}
			if e.IsDir {
				// prune subtrees that never contain playable video:
				// audio CDs, artwork scans, fonts, chapters, menus…
				if reJunkDir.MatchString(e.Name) {
					continue
				}
				wg.Add(1)
				go walkDir(e.Path, depth+1)
				continue
			}
			low := strings.ToLower(e.Name)
			if strings.HasSuffix(low, ".strm") {
				if url := readStrm(ctx, drv, e.Path); url != "" {
					mu.Lock()
					files = append(files, fileRec{Path: url, Dir: dir, Strm: true, SourceID: srcID, Root: root})
					mu.Unlock()
					filesFound.Add(1)
				}
				continue
			}
			if !IsVideo(e.Name) {
				continue
			}
			if e.Size > 0 && e.Size < 1<<20 {
				continue // skip tiny junk
			}
			mu.Lock()
			files = append(files, fileRec{Path: e.Path, Dir: dir, Size: e.Size, MTime: e.MTime, SourceID: srcID, Root: root})
			mu.Unlock()
			filesFound.Add(1)
		}
	}

	wg.Add(1)
	root = strings.TrimSuffix(root, "/")
	if root == "" {
		root = "/"
	}
	walkDir(root, 0)
	wg.Wait()
	return files, listings, firstErr
}

func readStrm(ctx context.Context, drv source.Driver, path string) string {
	rc, err := drv.Open(ctx, path)
	if err != nil {
		return ""
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, 64<<10))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			return line
		}
		if line != "" && !strings.HasPrefix(line, "#") {
			return ""
		}
	}
	return ""
}

// ---------- organize ----------

type movieGroup struct {
	Title string
	Year  int
	Files []fileRec
}

func (sc *Scanner) organize(lib *models.Library, files []fileRec, prog *models.ScanProgress, current, walked map[int64]bool) error {
	// The walk runs for minutes without touching the database, and the first
	// statement after it is the one that fails. Ping before any real work, so a
	// connection whose handle went stale during that idle stretch is replaced on
	// its own rather than under a query that matters.
	if err := sc.Store.DB.Ping(); err != nil {
		log.Printf("organize: ping before organize failed: %v", err)
	}

	switch lib.Type {
	case "series":
		err := sc.organizeSeries(lib, files, prog, current, walked)
		return err
	default:
		err := sc.organizeMovies(lib, files, prog, current, walked)
		return err
	}
}

func (sc *Scanner) organizeMovies(lib *models.Library, files []fileRec, prog *models.ScanProgress, current, walked map[int64]bool) error {
	existing, _, err := sc.Store.ItemsByLibrary(lib.ID, "movie", "", "", false, 0, 1000000)
	if err != nil {
		log.Printf("organize: load movies failed: %v", err)
		return err
	}
	byKey := map[string]int64{}
	for _, it := range existing {
		byKey[NormalizeKey(fmt.Sprintf("%s|%d", it.Name, it.Year))] = it.ID
	}

	groups := map[string]*movieGroup{}
	var order []string
	add := func(key, title string, year int, fr fileRec) {
		g, ok := groups[key]
		if !ok {
			g = &movieGroup{Title: title, Year: year}
			groups[key] = g
			order = append(order, key)
		}
		g.Files = append(g.Files, fr)
	}

	for _, fr := range files {
		if fr.Strm || fr.Path == fr.Dir || fr.Dir == strings.TrimSuffix(fr.Root, "/") {
			// flat file: parse by filename
			p := ParseName(baseName(fr.Path))
			add(groupKey(p), orDefault(p.Title, baseName(fr.Path)), p.Year, fr)
			continue
		}
		fp := ParseName(baseName(fr.Path))
		dp := ParseName(baseName(fr.Dir))
		if dp.Title != "" {
			year := dp.Year
			if year == 0 {
				year = fp.Year
			}
			add(groupKey(dp), dp.Title, year, fr)
		} else {
			add(groupKey(fp), orDefault(fp.Title, baseName(fr.Path)), fp.Year, fr)
		}
	}

	for i, key := range order {
		g := groups[key]
		if g.Title == "" {
			continue
		}
		itemID, ok := byKey[key]
		if !ok {
			it := &models.Item{LibraryID: lib.ID, Type: "movie", Name: g.Title, Year: g.Year}
			if err := sc.Store.InsertItem(it); err != nil {
				return err
			}
			itemID = it.ID
			byKey[key] = it.ID
		}
		for _, fr := range g.Files {
			if _, err := sc.Store.UpsertFile(&models.MediaFile{
				ItemID: itemID, SourceID: fr.SourceID, Path: fr.Path, Size: fr.Size, MTime: fr.MTime,
			}); err != nil {
				return err
			}
		}
		prog.Update(func(st *models.ScanState) {
			st.Done = i + 1
			st.Current = g.Title
		})
		prog.AddEvent("show", fmt.Sprintf("%s%s", g.Title, yearSuffix(g.Year)))
	}
	return sc.cleanup(lib, files, current, walked)
}

// yearSuffix renders " (2019)" for the feed, empty when the year is unknown.
func yearSuffix(year int) string {
	if year <= 0 {
		return ""
	}
	return fmt.Sprintf(" (%d)", year)
}

type seriesIndex struct {
	shows       map[string]int64 // normKey -> show id
	seasons     map[string]int64 // showID|season -> season id
	seasonNames map[string]int64 // showID|normName -> season id (sub-series/arc seasons)
	eps         map[string]int64 // seasonID|ep -> episode id
	maxSeason   map[int64]int    // showID -> highest regular season number in use
	loaded      map[int64]bool   // seasonID -> its episodes have been listed already
}

// nextSeason hands out the next free regular season number (1..90) for a show;
// 91+ are reserved for special collections (OAD/OVA/剧场版…).
func (si *seriesIndex) nextSeason(showID int64) int {
	if si.maxSeason == nil {
		si.maxSeason = map[int64]int{}
	}
	n := si.maxSeason[showID] + 1
	if n < 1 {
		n = 1
	}
	if n > 90 {
		n = 90
	}
	si.maxSeason[showID] = n
	return n
}

// registerSeason keeps every lookup map in sync after a season is created.
func (si *seriesIndex) registerSeason(showID int64, num int, name string, id int64) {
	si.seasons[fmt.Sprintf("%d|%d", showID, num)] = id
	if name != "" {
		si.seasonNames[fmt.Sprintf("%d|%s", showID, NormalizeKey(name))] = id
	}
	if num > 0 && num <= 90 && num > si.maxSeason[showID] {
		si.maxSeason[showID] = num
	}
}

// seasonKeyFor resolves an existing season item for a file, either by number
// (第 2 季 / OAD slots) or by name (sub-series and arc folders like 化物语).
func (si *seriesIndex) seasonKeyFor(showID int64, num int, name string) (int64, bool) {
	if num > 0 {
		id, ok := si.seasons[fmt.Sprintf("%d|%d", showID, num)]
		return id, ok
	}
	id, ok := si.seasonNames[fmt.Sprintf("%d|%s", showID, NormalizeKey(name))]
	return id, ok
}

// dirChildCount counts, for every directory, how many direct child
// directories hold something. A folder whose children are themselves show-like
// is a franchise/arc container (物语系列/{化物语,伪物语}), not a show.
func dirChildCount(files []fileRec) map[string]int {
	dirs := map[string]bool{}
	for _, fr := range files {
		d := strings.TrimSuffix(fr.Dir, "/")
		// walk up to the root so intermediate folders (the ones that hold no
		// file themselves) are counted as children of their parent too
		for d != "" && d != "/" && !dirs[d] {
			dirs[d] = true
			d = parentDir(d)
		}
	}
	count := map[string]int{}
	for d := range dirs {
		if p := parentDir(d); p != "" {
			count[p]++
		}
	}
	return count
}

// planShow resolves one file into (show directory, season number, season
// label). Season numbers are 0 when the season is identified by name only
// (a sub-series/arc folder that gets its own slot with a stable label).
func planShow(childCount map[string]int, libPath, dir, filePath string, parsedSeason int) (showDir string, seasonNum int, seasonLbl string) {
	showDir = showDirFor(libPath, dir)
	if showDir == "" {
		// file at the library root or directly inside a container folder: the
		// title comes from the file name, and no folder can mark a season
		return "", 1, "第 1 季"
	}

	// Franchise container: 物语系列/{化物语,伪物语}, 青之驱魔师/{岛根启明结社篇,…}.
	// The parent groups several show-like children, so it is the show and each
	// child becomes its own season, named after the folder.
	subLabel := ""
	childDir := ""
	if showDir != "" {
		parent := parentDir(showDir)
		if parent != "" &&
			strings.TrimSuffix(parent, "/") != strings.TrimSuffix(libPath, "/") &&
			!isTechDir(baseName(parent)) && !IsCategoryDir(baseName(parent)) &&
			childCount[parent] >= 2 {
			childDir = showDir
			subLabel = DisplayName(baseName(showDir))
			showDir = parent
		}
	}

	num, lbl, ok := seasonMetaFor(showDir, filePath)
	// A child folder that names its own season ("第一季", "Season 2") *is* that
	// season: the number orders it and the name labels it. Without this every
	// season of a franchise was a name-only slot with number 0 — bilibili numbers
	// its seasons, so they came out in an arbitrary order (第三季 before 第一季)
	// and their numbers were lost.
	childNum := 0
	if subLabel != "" {
		if n, ok := ParseSeasonDir(baseName(childDir)); ok {
			childNum = n
		}
	}
	switch {
	case subLabel != "" && childNum > 0:
		return showDir, childNum, subLabel
	case subLabel != "" && parsedSeason > 0:
		return showDir, 0, fmt.Sprintf("%s · 第 %d 季", subLabel, parsedSeason)
	case subLabel != "" && ok:
		return showDir, 0, subLabel + " · " + lbl
	case subLabel != "":
		return showDir, 0, subLabel
	case parsedSeason > 0:
		return showDir, parsedSeason, fmt.Sprintf("第 %d 季", parsedSeason)
	case ok:
		return showDir, num, lbl
	default:
		return showDir, 1, "第 1 季"
	}
}

func (sc *Scanner) organizeSeries(lib *models.Library, files []fileRec, prog *models.ScanProgress, current, walked map[int64]bool) error {
	idx := &seriesIndex{
		shows:       map[string]int64{},
		seasons:     map[string]int64{},
		seasonNames: map[string]int64{},
		eps:         map[string]int64{},
		maxSeason:   map[int64]int{},
		loaded:      map[int64]bool{},
	}

	shows, _, err := sc.Store.ItemsByLibrary(lib.ID, "show", "", "", false, 0, 100000)
	if err != nil {
		return err
	}
	for _, it := range shows {
		idx.shows[NormalizeKey(it.Name)] = it.ID
	}
	seasons, _, err := sc.Store.ItemsByLibrary(lib.ID, "season", "", "", false, 0, 200000)
	if err != nil {
		return err
	}
	for _, it := range seasons {
		if it.ParentID != nil {
			idx.seasons[fmt.Sprintf("%d|%d", *it.ParentID, it.Season)] = it.ID
			if it.Name != "" {
				idx.seasonNames[fmt.Sprintf("%d|%s", *it.ParentID, NormalizeKey(it.Name))] = it.ID
			}
			if it.Season > 0 && it.Season <= 90 && it.Season > idx.maxSeason[*it.ParentID] {
				idx.maxSeason[*it.ParentID] = it.Season
			}
		}
	}
	// Episodes are looked up per season rather than loaded wholesale. The one big
	// read a rescan used to make here is also the read that fails outright
	// ("unable to open database file") once the walk has run for a couple of
	// minutes — on a connection where smaller reads, including the very same
	// statement, keep working. Asking per season keeps every read small, and it
	// is less work anyway: a season's episodes are only needed when a file for
	// that season is being stored.
	//
	// The lookups are memoised in idx.eps, so a season is queried once and the
	// episodes created during this scan are visible to the next file without a
	// second query.
	loadSeasonEps := func(seasonID int64) {
		if idx.loaded[seasonID] {
			return
		}
		idx.loaded[seasonID] = true
		kids, err := sc.Store.ItemChildren(seasonID)
		if err != nil {
			// Not fatal: without the index the worst case is a duplicate episode
			// row for a season whose episodes could not be listed, which the
			// next scan's cleanup removes.
			log.Printf("organize: list episodes of season %d failed: %v", seasonID, err)
			return
		}
		for _, it := range kids {
			if it.Type == "episode" {
				idx.eps[fmt.Sprintf("%d|%d", seasonID, it.Episode)] = it.ID
			}
		}
	}
	_ = loadSeasonEps

	childCount := dirChildCount(files)

	// Plan every file first, then insert NUMBERED seasons (第 N 季 / OAD / 剧场版)
	// before named ones (化物语, 岛根启明结社篇…). Named seasons take the next free
	// slot, so a sub-series can never squat on "第 1 季" and swallow its episodes
	// as extra "versions".
	type planned struct {
		fr      fileRec
		show    string
		season  int
		label   string
		episode int
	}
	numbered := make([]planned, 0, len(files))
	named := make([]planned, 0, len(files))
	for _, fr := range files {
		p := ParseName(baseName(fr.Path))
		// extras / unparseable files (NCOP, NCED, menus, previews…) have no
		// episode number — keep them out of the library (still playable via
		// the file browser)
		if p.Episode == 0 {
			continue
		}
		// Show title: prefer the show DIRECTORY (the season-dir's parent), so
		// "风夏/Season 1/EP01.mkv" or "风夏/01.mkv" both resolve to show "风夏".
		// Only fall back to the filename when the file sits at the library root.
		showDir, season, seasonName := planShow(childCount, fr.Root, fr.Dir, fr.Path, p.Season)
		showTitle := ""
		if showDir != "" {
			dp := ParseName(baseName(showDir))
			showTitle = orDefault(dp.Title, baseName(showDir))
		}
		if showTitle == "" {
			showTitle = orDefault(stripTitleYear(p.Title), baseName(fr.Path))
		}
		pl := planned{fr: fr, show: showTitle, season: season, label: seasonName, episode: p.Episode}
		if season > 0 {
			numbered = append(numbered, pl)
		} else {
			named = append(named, pl)
		}
	}

	for _, pl := range append(numbered, named...) {
		fr, showTitle, season, seasonName, ep := pl.fr, pl.show, pl.season, pl.label, pl.episode

		showKey := NormalizeKey(showTitle)
		showID, ok := idx.shows[showKey]
		if !ok {
			it := &models.Item{LibraryID: lib.ID, Type: "show", Name: showTitle}
			if err := sc.Store.InsertItem(it); err != nil {
				log.Printf("organize: insert show %q failed: %v", showTitle, err)
				return err
			}
			showID = it.ID
			idx.shows[showKey] = showID
		}

		seasonID, ok := idx.seasonKeyFor(showID, season, seasonName)
		if ok && season > 0 {
			// A named season from an older scan may be sitting on this number:
			// move it out of the way instead of merging two season into one.
			if old, err := sc.Store.ItemByID(seasonID); err == nil && old.Name != seasonName &&
				!isNumericSeasonLabel(old.Name) {
				moved := idx.nextSeason(showID)
				old.Season = moved
				if err := sc.Store.UpdateItemMeta(old); err == nil {
					delete(idx.seasons, fmt.Sprintf("%d|%d", showID, season))
					idx.registerSeason(showID, moved, old.Name, seasonID)
					seasonID, ok = 0, false
				}
			}
		}
		switch {
		case !ok:
			if season <= 0 {
				season = idx.nextSeason(showID)
			}
			pid := showID
			it := &models.Item{LibraryID: lib.ID, Type: "season", ParentID: &pid, Name: seasonName, Season: season}
			if err := sc.Store.InsertItem(it); err != nil {
				return err
			}
			seasonID = it.ID
			idx.registerSeason(showID, season, seasonName, seasonID)
		default:
			if old, err := sc.Store.ItemByID(seasonID); err == nil {
				// An episode belongs to the number its season row carries. A name-only
				// season ("立志篇" / "游郭篇") arrives here with 0 and is handed a number
				// when its row is created — so the files that arrive after that row
				// exists have to take that number. They used to keep the 0, which put
				// every episode of the season in season 0 except the one that created
				// it, and a list ordered by season then shows the season's first
				// episode last.
				if old.Season > 0 {
					season = old.Season
				}
				if old.Name != seasonName {
					// keep the label in sync on rescans (e.g. "第 91 季" → "OAD")
					old.Name = seasonName
					_ = sc.Store.UpdateItemMeta(old)
				}
			}
		}

		epKey := fmt.Sprintf("%d|%d", seasonID, ep)
		epID, ok := idx.eps[epKey]
		if !ok {
			loadSeasonEps(seasonID)
			epID, ok = idx.eps[epKey]
		}
		if !ok {
			pid := seasonID
			it := &models.Item{LibraryID: lib.ID, Type: "episode", ParentID: &pid,
				Name: fmt.Sprintf("第 %d 集", ep), Season: season, Episode: ep}
			if err := sc.Store.InsertItem(it); err != nil {
				return err
			}
			epID = it.ID
			idx.eps[epKey] = epID
		} else if cur, err := sc.Store.ItemByID(epID); err == nil && (cur.Season != season || cur.Episode != ep) {
			// Repair what an older scan stored: an episode written as season 0 sorts
			// to the end of its own season, so a rescan has to correct the row rather
			// than leave the library in the state that was wrong to begin with.
			cur.Season, cur.Episode = season, ep
			_ = sc.Store.UpdateItemMeta(cur)
		}

		if _, err := sc.Store.UpsertFile(&models.MediaFile{
			ItemID: epID, SourceID: fr.SourceID, Path: fr.Path, Size: fr.Size, MTime: fr.MTime,
		}); err != nil {
			log.Printf("organize: UpsertFile failed (done=%d) path=%q: %v", progDone(prog), fr.Path, err)
			return err
		}
		prog.Update(func(st *models.ScanState) {
			st.Done++
			st.Current = fmt.Sprintf("%s S%02dE%02d", showTitle, season, ep)
		})
		prog.AddEvent("episode", fmt.Sprintf("%s S%02dE%02d", showTitle, season, ep))

	}
	if err := sc.cleanup(lib, files, current, walked); err != nil {
		log.Printf("organize: cleanup failed (done=%d): %v", progDone(prog), err)
		return err
	}
	return nil
}

// progDone and progTotal read the organizer's counters, for diagnostics.
func progDone(prog *models.ScanProgress) int {
	st, _ := prog.Snapshot(0)
	return st.Done
}

// cleanup deletes files no longer present and prunes childless items.
// cleanup deletes the files a library no longer holds, and the items left empty.
//
// A file is identified by the source it came from *and* its path: a library may
// scan several sources, and the same path in two of them is two different files.
//
// current is the set of sources this library scans now, walked the set this scan
// managed to read, and the difference between them is the whole point:
//
//   - a file whose source is no longer one of the library's is deleted: the
//     library stopped scanning there, so those rows belong to nobody;
//   - a file whose source *is* one of them but was not read this time (a listing
//     that failed, a drive that was asleep) is left alone — not being able to
//     read a source is not evidence that its files are gone, and deleting them
//     would take the rows and the watch progress attached to them with it.
func (sc *Scanner) cleanup(lib *models.Library, files []fileRec, current, walked map[int64]bool) error {
	seen := map[string]bool{}
	for _, fr := range files {
		seen[fileSourceKey(fr.SourceID, fr.Path)] = true
	}
	rows, err := sc.Store.DB.Query(`SELECT f.id, f.source_id, f.path FROM files f JOIN items i ON i.id = f.item_id WHERE i.library_id = ?`, lib.ID)
	if err != nil {
		return err
	}
	var gone []int64
	for rows.Next() {
		var id, srcID int64
		var p string
		if err := rows.Scan(&id, &srcID, &p); err != nil {
			rows.Close()
			return err
		}
		if !current[srcID] {
			gone = append(gone, id)
			continue
		}
		if !walked[srcID] {
			continue
		}
		if !seen[fileSourceKey(srcID, p)] {
			gone = append(gone, id)
		}
	}
	rows.Close()
	for _, id := range gone {
		if _, err := sc.Store.DB.Exec(`DELETE FROM files WHERE id=?`, id); err != nil {
			return err
		}
	}
	stmts := []string{
		`DELETE FROM items WHERE library_id=? AND type IN ('movie','episode') AND id NOT IN (SELECT DISTINCT item_id FROM files)`,
		`DELETE FROM items WHERE library_id=? AND type='season' AND id NOT IN (SELECT DISTINCT parent_id FROM items WHERE parent_id IS NOT NULL AND library_id=?)`,
		`DELETE FROM items WHERE library_id=? AND type='show' AND id NOT IN (SELECT DISTINCT parent_id FROM items WHERE parent_id IS NOT NULL AND library_id=?)`,
		// second pass in case shows became empty after seasons were pruned
		`DELETE FROM items WHERE library_id=? AND type IN ('movie','episode') AND id NOT IN (SELECT DISTINCT item_id FROM files)`,
	}
	for _, stmt := range stmts {
		if strings.Count(stmt, "?") == 1 {
			if _, err := sc.Store.DB.Exec(stmt, lib.ID); err != nil {
				return err
			}
		} else {
			if _, err := sc.Store.DB.Exec(stmt, lib.ID, lib.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// fileSourceKey identifies a file within a library: the same path on two sources
// is two different files.
func fileSourceKey(sourceID int64, path string) string {
	return fmt.Sprintf("%d|%s", sourceID, path)
}
func groupKey(p Parsed) string {
	return NormalizeKey(fmt.Sprintf("%s|%d", p.Title, p.Year))
}

func baseName(path string) string {
	p := strings.TrimSuffix(path, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// parentDir returns the directory above dir, or "" when there is none.
func parentDir(dir string) string {
	p := strings.TrimSuffix(dir, "/")
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return ""
	}
	return p[:i]
}

// showDirFor finds the "show" directory for an episode file at dir.
// Real collections nest files deep:
//
//	剧名/第 1 季/bdrip/[压制组]/xx.mkv
//	剧名/bdrip/第 1 季/xx.mkv
//	剧名/OAD/xx.mkv
//
// so climb from dir toward the library root:
//   - a season-like segment (第 N 季 / Season N / S01 / OAD / 剧场版 / SP…) marks
//     the split between show and season: the show is the nearest NON-technical
//     ancestor above it (skipping bdrip/webrip/1080p/release-group dirs)
//   - with no season-like segment, skip technical dirs and take the first
//     non-technical ancestor
//   - fall back to the first directory below the library root
//
// Returns "" when the file sits directly in the library root (title must
// come from the filename then).
func showDirFor(libPath, dir string) string {
	norm := func(p string) string {
		p = strings.TrimSuffix(p, "/")
		return strings.TrimSuffix(p, "\\")
	}
	lib := norm(libPath)
	d := norm(dir)
	if d == "" || d == "." || d == lib {
		return ""
	}
	segs := strings.Split(strings.TrimPrefix(d, "/"), "/")
	join := func(n int) string { return "/" + strings.Join(segs[:n], "/") }

	for i := len(segs) - 1; i >= 1; i-- {
		if IsContainerDir(segs[i]) {
			continue
		}
		_, isSeason := ParseSeasonDir(segs[i])
		_, _, isSpecial := SpecialSeasonDir(segs[i])
		if !isSeason && !isSpecial && !isNumericDir(segs[i]) {
			continue
		}
		j := i - 1
		for j >= 1 && (isTechDir(segs[j]) || isNumericDir(segs[j])) {
			j--
		}
		if j >= 1 {
			return cleanShowDir(join(j+1), lib)
		}
		break
	}
	for i := len(segs) - 1; i >= 1; i-- {
		if !isTechDir(segs[i]) && !isNumericDir(segs[i]) && !IsContainerDir(segs[i]) {
			return cleanShowDir(join(i+1), lib)
		}
	}
	if len(segs) >= 2 {
		return cleanShowDir(join(2), lib)
	}
	return cleanShowDir("/"+segs[0], lib)
}

// cleanShowDir rejects a "show" that is really the library root or a container
// folder (a file directly inside /Cartoon/Movie has no show at all): the caller
// then falls back to the file name instead of inventing a "Cartoon" show.
func cleanShowDir(showDir, lib string) string {
	if strings.TrimSuffix(showDir, "/") == lib || IsContainerDir(baseName(showDir)) {
		return ""
	}
	return showDir
}

// isNumericDir matches structure folders like "1", "01", "0" — used both as
// bare season markers (剧名/bdrip/0/…) and as per-episode folders
// (剧名/01/…), so they must never become the show title themselves.
func isNumericDir(name string) bool {
	n := strings.TrimSpace(name)
	if n == "" || len(n) > 3 {
		return false
	}
	for _, r := range n {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// resolveSeason turns the parser's season (0 when the file name carries none)
// plus the directory structure into the stored season number + display label.
// A directory that explicitly says "0" keeps season 0 as its own slot instead
// of being folded into season 1 (剧名/bdrip/0 vs 剧名/bdrip/<2011 release>).
// seasonMetaFor resolves the season number + display name for an episode file.
// Only directories *below* showDir are considered (never the show dir itself
// or the file name), so a show called "86" does not turn into season 86:
// numeric dirs ("第 2 季"/"Season 2"/"S02"/"0") get a "第 N 季" label, special
// collections (OAD/OVA/剧场版/SP/特典…) get their own slot and keep their name.
func seasonMetaFor(showDir, filePath string) (int, string, bool) {
	segs := strings.Split(strings.Trim(filePath, "/"), "/")
	start := 0
	if sd := strings.Trim(showDir, "/"); sd != "" {
		if n := len(strings.Split(sd, "/")); n < len(segs) {
			start = n
		}
	}
	for i := len(segs) - 2; i >= start; i-- {
		if n, ok := ParseSeasonDir(segs[i]); ok {
			return n, fmt.Sprintf("第 %d 季", n), true
		}
		if disp, num, ok := SpecialSeasonDir(segs[i]); ok {
			return num, disp, true
		}
		if isNumericDir(segs[i]) {
			n, _ := strconv.Atoi(strings.TrimSpace(segs[i]))
			return n, fmt.Sprintf("第 %d 季", n), true
		}
	}
	return 0, "", false
}

var reTechDir = regexp.MustCompile(`(?i)(bdrip|web[\.\-_ ]?rip|web[\.\-_ ]?dl|blu[\.\-_ ]?ray|remux|bdmux|\buhd\b|ma10p|hi10p|8bit|10bit|x26[45]|h[\.\-_ ]?26[45]|hevc|\bavc\b|1080[pi]|720p|2160p|4320p|flac|\baac\b)`)

// reJunkDir also covers subtitle/language folders (简体 / CHS / BIG5 …): they
// sit next to the video files and must never become a title or a season.
var reJunkDir = regexp.MustCompile(`(?i)^(cds?|scans?|extras?|featurettes?|menus?|others?|backup|subs?|subtitles|chapters?|fonts?|ncops?|nceds?|pvs?|cms?|previews?|trailers?|samples?|omake|bonuses?|bonus|简体|繁體|繁体|简繁|繁简|简日|繁日|chs|cht|big5|gb|sc|tc)$`)

// reNumericSeasonLabel matches a plain "第 N 季" label: such a season is
// number-keyed and may be renamed in place, while a sub-series season
// (化物语 / 岛根启明结社篇) is keyed by its name and gets moved instead.
var reNumericSeasonLabel = regexp.MustCompile(`^第 \d+ 季$`)

func isNumericSeasonLabel(name string) bool {
	return reNumericSeasonLabel.MatchString(strings.TrimSpace(name))
}

// isTechDir: release-group / quality / junk directories that must not be
// mistaken for the show title.
func isTechDir(name string) bool {
	if reJunkDir.MatchString(name) || reTechDir.MatchString(name) {
		return true
	}
	// release-group style: two or more bracketed groups
	return len(reBracketGroups.FindAllString(name, -1)) >= 2
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
