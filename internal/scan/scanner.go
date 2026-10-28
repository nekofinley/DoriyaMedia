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
	// SrcType is the kind of source the file was listed by ("bilibili", "local",
	// …). Some sources already name what they list — bilibili states every
	// episode's title and marks its section entries — and that naming is better
	// than anything this package can infer from a path.
	SrcType string
}

type Scanner struct {
	Store      *db.Store
	Cfg        *config.Config
	OnIdentify func(lib *models.Library, listings map[string][]source.Entry, report func(done, total int, msg string)) // metadata enrichment hook
	// OnScanned fires after a library scan finishes, so the caller can warm
	// caches (e.g. pre-probe new files) without blocking the scan itself.
	OnScanned func(lib *models.Library)
	// Path scans use scoped hooks so enrichment and pre-probing do not expand
	// back into a full-library operation.
	OnIdentifyFiles func(lib *models.Library, files []models.MediaFile, listings map[string][]source.Entry, report func(done, total int, msg string))
	OnScannedFiles  func(lib *models.Library, files []models.MediaFile)
	states          sync.Map // libraryID -> *models.ScanProgress
	cancels         sync.Map // libraryID -> context.CancelFunc

	queue chan scanJob
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
	return sc.enqueue(scanJob{lib: lib})
}

func (sc *Scanner) enqueue(job scanJob) error {
	sc.once.Do(func() {
		sc.queue = make(chan scanJob, 64)
		go sc.worker()
	})
	select {
	case sc.queue <- job:
		return nil
	default:
		return ErrScanQueueFull
	}
}

func (sc *Scanner) worker() {
	for job := range sc.queue {
		_ = sc.runScanScoped(job.lib, job.scope)
	}
}

func (sc *Scanner) runScan(lib *models.Library) error {
	return sc.runScanScoped(lib, nil)
}

func (sc *Scanner) runScanScoped(lib *models.Library, scope *scanScope) error {
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

	if scope != nil {
		// A queued scan must honor configuration changes made while it waited.
		fresh, err := sc.Store.LibraryByID(lib.ID)
		if err != nil {
			fail("媒体库已不存在: " + err.Error())
			return err
		}
		resolved, err := sc.resolveScanScope(fresh, scope.SourceID, scope.Path)
		if err != nil {
			fail(err.Error())
			return err
		}
		lib, scope = fresh, resolved
	}

	// Where this library scans. A library may hold several places; each is walked
	// with its own driver, and every file found keeps the source it came from.
	places, err := sc.Store.LibrarySources(lib.ID)
	if err != nil || len(places) == 0 {
		places = []models.LibrarySource{{LibraryID: lib.ID, SourceID: lib.SourceID, Path: lib.Path}}
	}
	if scope != nil {
		places = []models.LibrarySource{{LibraryID: lib.ID, SourceID: scope.SourceID, Path: scope.Root}}
		prog.AddEvent("info", "仅扫描路径 "+scope.Path)
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
	var walkSeen atomic.Int64
	onDir := func(dir string, dirs, found, errs int64, firstErr string) {
		n := walkSeen.Add(1)
		prog.Update(func(st *models.ScanState) {
			st.Current = dir
			st.DirsDone, st.FilesFound, st.WalkErrs = int(dirs), int(found), int(errs)
			if firstErr != "" && st.FirstErr == "" {
				st.FirstErr = firstErr
			}
		})
		if n%8 == 0 {
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
	// partial marks sources whose top-level listing is a sample (bilibili's
	// feed), not an inventory. Cleanup must not read "not in the root listing"
	// as "deleted" for those.
	partial := map[int64]bool{}
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
		if scope != nil {
			if err := scope.validateDriver(drv); err != nil {
				srcErrs = append(srcErrs, fmt.Sprintf("%s: %v", src.Name, err))
				continue
			}
		}
		if pl, ok := drv.(source.PartialLister); ok && pl.PartialListing() {
			partial[place.SourceID] = true
		}
		start := place.Path
		if scope != nil {
			start = scope.Path
		}
		prog.AddEvent("info", fmt.Sprintf("扫描 %s %s", src.Name, start))
		// Keep the configured root for naming, even when walking a deep subtree.
		f, l, werr := walkTree(ctx, drv, place.Path, start, sc.Cfg.ScanConcurrency, place.SourceID, src.Type, onDir, scope)
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

	if scope != nil {
		files, err = sc.keepScopedFiles(lib, files, prog)
		if err != nil {
			fail("检查文件归属失败: " + err.Error())
			return err
		}
	}
	prog.Update(func(st *models.ScanState) {
		st.Phase = "organize"
		st.Total = len(files)
		st.Done = 0
		st.Current = fmt.Sprintf("发现 %d 个视频文件,正在整理…", len(files))
	})
	prog.AddEvent("file", fmt.Sprintf("发现 %d 个视频文件", len(files)))
	if err := sc.organize(lib, files, prog, current, walked, partial, listings, scope); err != nil {
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
	report := func(done, total int, msg string) {
		prog.Update(func(st *models.ScanState) {
			st.Done, st.Total, st.Current = done, total, msg
		})
		if msg != "" {
			prog.AddEvent("meta", msg)
		}
	}
	var scannedFiles []models.MediaFile
	if scope != nil {
		scannedFiles = sc.scannedFiles(lib, files)
		if sc.OnIdentifyFiles != nil {
			sc.OnIdentifyFiles(lib, scannedFiles, listings, report)
		}
	} else if sc.OnIdentify != nil {
		sc.OnIdentify(lib, listings, report)
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
	if scope != nil {
		if sc.OnScannedFiles != nil {
			sc.OnScannedFiles(lib, scannedFiles)
		}
	} else if sc.OnScanned != nil {
		sc.OnScanned(lib)
	}
	return nil
}

// ImportSeries scans one series directory into an existing library, without
// walking the rest of the source. This is what makes "search a title, click it,
// import it" possible without scanning bilibili's whole catalogue (thousands of
// seasons, and a rate limit that such a walk would trip).
//
// The walk is a partial listing by definition: cleanup is told so, and can only
// remove files under directories this import actually read. Rows of every other
// show in the library are untouched.
func (sc *Scanner) ImportSeries(ctx context.Context, lib *models.Library, sourceID int64, seriesPath string, prog *models.ScanProgress) (int64, error) {
	src, err := sc.Store.SourceByID(sourceID)
	if err != nil {
		return 0, err
	}
	drv, err := source.NewDriver(src)
	if err != nil {
		return 0, err
	}
	if prog == nil {
		prog = models.NewScanProgress(lib.ID)
	}
	prog.Update(func(st *models.ScanState) {
		st.LibraryID, st.Running, st.Phase, st.Current = lib.ID, true, "walk", seriesPath
	})
	onDir := func(dir string, dirs, found, errs int64, firstErr string) {
		prog.Update(func(st *models.ScanState) {
			st.Current = dir
			st.DirsDone, st.FilesFound, st.WalkErrs = int(dirs), int(found), int(errs)
			if firstErr != "" && st.FirstErr == "" {
				st.FirstErr = firstErr
			}
		})
	}
	// The series directory's parent is the library root as far as the scanner's
	// naming is concerned: with "/" as the root, the series directory is the
	// show and each [season N] child is one of its seasons.
	root := parentDir(seriesPath)
	if root == "" {
		root = "/"
	}
	files, listings, werr := walkTree(ctx, drv, root, seriesPath, sc.Cfg.ScanConcurrency, sourceID, src.Type, onDir)
	if len(files) == 0 {
		if werr != nil {
			return 0, fmt.Errorf("读取番剧失败: %w", werr)
		}
		if src.Type == "bilibili" {
			// With the per-source "只上架当前线路能播的番剧" switch on, a
			// season licensed to another region is listed as empty on purpose.
			// Say so instead of the generic "nothing playable", or importing a
			// 港澳台 title through a mainland proxy reads like a broken source.
			return 0, fmt.Errorf("没有读到可播放的剧集:该番剧可能不在当前代理地区(换一个落地地区,或关闭该源的「只上架当前线路能播的番剧」再试)")
		}
		return 0, fmt.Errorf("没有读到可播放的剧集")
	}
	prog.Update(func(st *models.ScanState) {
		st.Phase, st.Done, st.Total = "organize", 0, len(files)
	})
	// The import is a subtree even for a source that calls itself exhaustive:
	// everything outside it is simply not part of this read.
	current := map[int64]bool{sourceID: true}
	walked := map[int64]bool{sourceID: true}
	partial := map[int64]bool{sourceID: true}
	if err := sc.organize(lib, files, prog, current, walked, partial, listings); err != nil {
		return 0, err
	}
	return sc.showOfFile(sourceID, files[0].Path), nil
}

// showOfFile walks one imported file back up to its show.
func (sc *Scanner) showOfFile(sourceID int64, path string) int64 {
	f, err := sc.Store.FileBySourcePath(sourceID, path)
	if err != nil || f == nil {
		return 0
	}
	ep, err := sc.Store.ItemByID(f.ItemID)
	if err != nil || ep.ParentID == nil {
		return 0
	}
	season, err := sc.Store.ItemByID(*ep.ParentID)
	if err != nil || season.ParentID == nil {
		return 0
	}
	return *season.ParentID
}

const maxDepth = 8

func walkTree(ctx context.Context, drv source.Driver, root, start string, concurrency int, srcID int64, srcType string, onDir func(dir string, dirs, files, errs int64, firstErr string), scopes ...*scanScope) ([]fileRec, map[string][]source.Entry, error) {
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
			if len(scopes) > 0 && scopes[0] != nil {
				p, err := cleanScanPath(e.Path)
				if err != nil || e.Name == "" || e.Name == "." || e.Name == ".." || strings.ContainsAny(e.Name, "/\\") || !pathWithin(p, scopes[0].Path) || source.VirtualJoin(dir, e.Name) != p {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("存储源返回了扫描目录外或非直接子项的路径: %s", e.Path)
					}
					mu.Unlock()
					errCount.Add(1)
					continue
				}
				e.Path = p
			}
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
					files = append(files, fileRec{Path: url, Dir: dir, Strm: true, SourceID: srcID, Root: root, SrcType: srcType})
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
			files = append(files, fileRec{Path: e.Path, Dir: dir, Size: e.Size, MTime: e.MTime, SourceID: srcID, Root: root, SrcType: srcType})
			mu.Unlock()
			filesFound.Add(1)
		}
	}

	wg.Add(1)
	root = strings.TrimSuffix(root, "/")
	if root == "" {
		root = "/"
	}
	start = strings.TrimSuffix(start, "/")
	if start == "" {
		start = "/"
	}
	walkDir(start, 0)
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

func (sc *Scanner) organize(lib *models.Library, files []fileRec, prog *models.ScanProgress, current, walked, partial map[int64]bool, listings map[string][]source.Entry, scopes ...*scanScope) error {
	// The walk runs for minutes without touching the database, and the first
	// statement after it is the one that fails. Ping before any real work, so a
	// connection whose handle went stale during that idle stretch is replaced on
	// its own rather than under a query that matters.
	if err := sc.Store.DB.Ping(); err != nil {
		log.Printf("organize: ping before organize failed: %v", err)
	}

	// What this library refuses to import: release furniture, and whatever the
	// user added to either list — file-name keywords, or folder names skipped
	// whole. Dropping the files here — before anything is planned — is what keeps
	// them out of the library *and* lets cleanup take back a file whose keyword
	// was added after it was imported.
	files = dropExcluded(files,
		excludeMatcher(EffectiveExcludes(lib, sc.Cfg)),
		excludeMatcher(EffectiveExcludeFolders(lib, sc.Cfg)))

	switch lib.Type {
	case "series":
		err := sc.organizeSeries(lib, files, prog, current, walked, partial, listings, scopes...)
		return sc.finishOrganize(lib, err)
	default:
		err := sc.organizeMovies(lib, files, prog, current, walked, partial, listings, scopes...)
		return sc.finishOrganize(lib, err)
	}
}

// finishOrganize closes an organize pass by recomputing the source type each
// item is marked with. The scan knows the type of every file it stored, but a
// show or season only learns that all of its episodes come from one kind of
// source after they are all in — this is the one place that sees that.
func (sc *Scanner) finishOrganize(lib *models.Library, err error) error {
	if err == nil {
		if terr := sc.Store.RefreshItemSourceTypes(lib.ID); terr != nil {
			log.Printf("organize: refresh item source types for library %d failed: %v", lib.ID, terr)
		}
	}
	return err
}

func (sc *Scanner) organizeMovies(lib *models.Library, files []fileRec, prog *models.ScanProgress, current, walked, partial map[int64]bool, listings map[string][]source.Entry, scopes ...*scanScope) error {
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
			add(groupKey(p), orDefault(p.Title, displayBaseName(fr.Path)), p.Year, fr)
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
			add(groupKey(fp), orDefault(fp.Title, displayBaseName(fr.Path)), fp.Year, fr)
		}
	}

	for i, key := range order {
		g := groups[key]
		if g.Title == "" {
			continue
		}
		itemID, ok := byKey[key]
		if !ok {
			st := ""
			if len(g.Files) > 0 {
				st = g.Files[0].SrcType
			}
			it := &models.Item{LibraryID: lib.ID, Type: "movie", Name: g.Title, Year: g.Year, SourceType: st}
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
	return sc.cleanup(lib, files, current, walked, partial, listings, scopes...)
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
// The name is also tried when a number is known: a season that an older scan
// stored under a free slot (第二季（后半） sat on "第 9 季") is still the season
// the folder names, and finding it lets the rescan correct its number instead
// of creating a second copy of it.
func (si *seriesIndex) seasonKeyFor(showID int64, num int, name string) (int64, bool) {
	if num > 0 {
		if id, ok := si.seasons[fmt.Sprintf("%d|%d", showID, num)]; ok {
			return id, true
		}
	}
	if name != "" {
		id, ok := si.seasonNames[fmt.Sprintf("%d|%s", showID, NormalizeKey(name))]
		return id, ok
	}
	return 0, false
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
	// The show folder itself may carry the season, the way a collection keeps one
	// show's seasons in sibling directories: "[CLANNAD][S2]", "[中二病也要谈恋爱!]
	// [S2][恋!]". Without this every season of such a show collapsed into 第 1 季
	// and two seasons that both number an episode 11 became one card with two
	// files — one of the two unreachable.
	dirSeason, dirLabel := 0, ""
	if _, ds, dn, dm := ParseShowDirName(baseName(showDir)); dm || ds > 0 || dn != "" {
		switch {
		case dm:
			// "[S0]" is the show's film, not a zeroth season.
			dirSeason, dirLabel = specialSeasonBase["movie"], "剧场版"
		case ds >= 91 && dn != "":
			// A special collection ("[OVA]") is its own card, named by itself.
			dirSeason, dirLabel = ds, dn
		case ds > 0 && dn != "":
			dirSeason, dirLabel = ds, fmt.Sprintf("第 %d 季 · %s", ds, dn)
		case ds > 0:
			dirSeason, dirLabel = ds, fmt.Sprintf("第 %d 季", ds)
		case dn != "":
			dirLabel = dn
		}
	}
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
	case dirSeason > 0 || dirLabel != "":
		return showDir, dirSeason, dirLabel
	default:
		return showDir, 1, "第 1 季"
	}
}

func (sc *Scanner) organizeSeries(lib *models.Library, files []fileRec, prog *models.ScanProgress, current, walked, partial map[int64]bool, listings map[string][]source.Entry, scopes ...*scanScope) error {
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

	// Every file a previous scan organized, mapped to the show that held it. The
	// provider titles written by the metadata pass are not stable across scans
	// ("装甲重拳 MEGALOBOX" becomes "MEGALO BOX"), so a show found by name is not
	// the same show. A file's path is: it is what the source listed, and what the
	// library keyed its rows by. Following it keeps one folder on one show across
	// a rename instead of creating a second show on every scan.
	showByFile, splitShows, err := sc.Store.FileShowIndex(lib.ID)
	if err != nil {
		log.Printf("organize: file → show index for library %d failed: %v", lib.ID, err)
		showByFile = map[string]int64{}
	}
	// A show that holds files from several source folders is a grouping an
	// older scan got wrong (one "TV" show for every single-season bilibili
	// series). Do not anchor to it: let the files be planned into their real
	// series, and let cleanup remove the empty remains.
	for key, showID := range showByFile {
		if splitShows[showID] {
			delete(showByFile, key)
		}
	}

	childCount := dirChildCount(files)

	// Plan every file first, then insert NUMBERED seasons (第 N 季 / OAD / 剧场版)
	// before named ones (化物语, 岛根启明结社篇…). Named seasons take the next free
	// slot, so a sub-series can never squat on "第 1 季" and swallow its episodes
	// as extra "versions".
	type planned struct {
		fr      fileRec
		show    string
		showID  int64 // the show this file already belonged to, when known
		season  int
		label   string
		episode int
		// episodeName is the name the source itself gives this entry, when it
		// gives one. It is better than a provider's guess about which episode of
		// which season this is, and it is what the card should say.
		episodeName string
	}
	numbered := make([]planned, 0, len(files))
	named := make([]planned, 0, len(files))
	// The files that actually become episodes, and so the only ones a series
	// library imports. Everything the organizer skips below is left out, which is
	// also what the final cleanup uses to purge a row a previous scan may have
	// imported for a file that no longer belongs here.
	imported := make([]fileRec, 0, len(files))
	var skippedFiles []fileRec
	for _, fr := range files {
		p := ParseName(baseName(fr.Path))
		// A file with no episode number is not an episode of this show. The
		// standard layout is "name(year)/Sxx/…" for a show and a separate
		// "name (year)" folder for a theatre film (kept in a movie library, not
		// this one). Such a file does not match a series library, so it is not
		// imported at all: importing it as a movie card is what let a bare title
		// like "Another" be scraped into the unrelated film "(an)other".
		//
		// Release furniture (NCOP/NCED, menus, previews) never reaches this loop:
		// SkipEntry drops those files during the walk.
		if p.Episode == 0 {
			skippedFiles = append(skippedFiles, fr)
			continue
		}
		imported = append(imported, fr)
		// Show title: prefer the show DIRECTORY (the season-dir's parent), so
		// "风夏/Season 1/EP01.mkv" or "风夏/01.mkv" both resolve to show "风夏".
		// Only fall back to the filename when the file sits at the library root.
		showDir, season, seasonName := planShow(childCount, fr.Root, fr.Dir, fr.Path, p.Season)
		showTitle := ""
		if showDir != "" {
			// A folder in the "[show][S2][季名]" convention names the show in its
			// first bracket; the later ones are the season, never the title.
			if t, _, _, _ := ParseShowDirName(baseName(showDir)); t != "" {
				showTitle = t
			} else {
				dp := ParseName(baseName(showDir))
				showTitle = orDefault(dp.Title, baseName(showDir))
			}
		}
		if showTitle == "" {
			showTitle = orDefault(stripTitleYear(p.Title), displayBaseName(fr.Path))
		}
		epName := ""
		if p.Extra {
			// A section entry: its own card, named after the section bilibili put
			// it in, and never a numbered episode of the season. This is the one
			// thing that matters about it — the season's episode 3 is a different
			// file, and the preview of it must not be offered as its "other
			// version" (which is what played when both were one card).
			season, seasonName = 0, p.ExtraName
			if lbl := sourceLabel(fr, showDir); lbl != "" {
				epName = lbl
			}
		} else {
			epName = sourceLabel(fr, showDir)
		}
		pl := planned{fr: fr, show: showTitle, showID: showByFile[fileSourceKey(fr.SourceID, fr.Path)],
			season: season, label: seasonName, episode: p.Episode, episodeName: epName}
		if season > 0 {
			numbered = append(numbered, pl)
		} else {
			named = append(named, pl)
		}
	}

	// A file that was already organized names its show; hand the same answer to
	// the files of that show which are new this scan, so one folder cannot come
	// out as an old show and a fresh one at the same time.
	for _, pl := range append(numbered, named...) {
		if pl.showID != 0 {
			idx.shows[NormalizeKey(pl.show)] = pl.showID
		}
	}

	for _, pl := range append(numbered, named...) {
		fr, showTitle, season, seasonName, ep := pl.fr, pl.show, pl.season, pl.label, pl.episode
		epName := pl.episodeName

		showKey := NormalizeKey(showTitle)
		showID, ok := pl.showID, pl.showID != 0
		if !ok {
			showID, ok = idx.shows[showKey]
		}
		if !ok {
			it := &models.Item{LibraryID: lib.ID, Type: "show", Name: showTitle, SourceType: fr.SrcType}
			if err := sc.Store.InsertItem(it); err != nil {
				log.Printf("organize: insert show %q failed: %v", showTitle, err)
				return err
			}
			showID = it.ID
			idx.shows[showKey] = showID
		}

		seasonID, ok := idx.seasonKeyFor(showID, season, seasonName)
		if ok && season > 0 {
			if old, err := sc.Store.ItemByID(seasonID); err == nil {
				// A season located by its name may still carry the free number an
				// older scan gave it; the number in the folder is the real one.
				if old.Season != season {
					delete(idx.seasons, fmt.Sprintf("%d|%d", showID, old.Season))
					old.Season = season
					if err := sc.Store.UpdateItemMeta(old); err == nil {
						idx.registerSeason(showID, season, old.Name, seasonID)
					}
				}
				if old.Name != seasonName && !isNumericSeasonLabel(old.Name) {
					loadSeasonEps(seasonID)
					clashEP, clash := idx.eps[fmt.Sprintf("%d|%d", seasonID, ep)]
					// The same file under the same season is not a clash — it is
					// this show being scanned again. A clash is another file on
					// the same episode number (中配 / 新编集版), which is what has
					// to keep its own card.
					sameFile := false
					if clash {
						if fls, err := sc.Store.FilesByItem(clashEP); err == nil {
							for _, f := range fls {
								if f.SourceID == fr.SourceID && f.Path == fr.Path {
									sameFile = true
									break
								}
							}
						}
					}
					if clash && !sameFile {
						// Another cut of this season: give it its own slot instead
						// of merging two editions into one card.
						moved := idx.nextSeason(showID)
						old.Season = moved
						if err := sc.Store.UpdateItemMeta(old); err == nil {
							delete(idx.seasons, fmt.Sprintf("%d|%d", showID, season))
							idx.registerSeason(showID, moved, old.Name, seasonID)
							seasonID, ok = 0, false
						}
					} else {
						// 第二季（前半）+ 第二季（后半） are one season told in two
						// parts: the episode numbers do not overlap, so they belong
						// on one card. Without this every second half was pushed to
						// a new slot ("第 9 季") beside its own first half.
						merged := mergeSeasonLabels(old.Name, seasonName)
						if merged != "" && merged != old.Name {
							old.Name = merged
							_ = sc.Store.UpdateItemMeta(old)
						}
						// Do not let the label sync below overwrite a card that now
						// describes both halves with this file's half.
						seasonName = old.Name
					}
				}
			}
		}
		switch {
		case !ok:
			if season <= 0 {
				season = idx.nextSeason(showID)
			}
			pid := showID
			it := &models.Item{LibraryID: lib.ID, Type: "season", ParentID: &pid, Name: seasonName, Season: season, SourceType: fr.SrcType}
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
				Name: orDefault(epName, fmt.Sprintf("第 %d 集", ep)), Season: season, Episode: ep, SourceType: fr.SrcType}
			if err := sc.Store.InsertItem(it); err != nil {
				return err
			}
			epID = it.ID
			idx.eps[epKey] = epID
		} else if cur, err := sc.Store.ItemByID(epID); err == nil {
			changed := false
			if cur.Season != season || cur.Episode != ep {
				// Repair what an older scan stored: an episode written as season 0 sorts
				// to the end of its own season, so a rescan has to correct the row rather
				// than leave the library in the state that was wrong to begin with.
				cur.Season, cur.Episode = season, ep
				changed = true
			}
			if epName != "" && cur.Name != epName {
				// The source names this entry, so take its name — including over a
				// name a provider guessed by episode number, which is how a second
				// season came to carry the first season's titles. A rescan is what
				// repairs those rows.
				cur.Name = epName
				changed = true
			}
			if changed {
				_ = sc.Store.UpdateItemMeta(cur)
			}
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

	// Files that do not match a series library (no episode number) are not
	// imported. Drop any row a previous scan wrote for them — the file may have
	// been imported before this rule existed, or under a different naming — so
	// cleanup can then take away the item it left behind.
	for _, fr := range skippedFiles {
		if len(scopes) > 0 && scopes[0] != nil {
			// Scoped cleanup removes this row using the directory listing and
			// can then prune just its ancestors, without a global orphan sweep.
			continue
		}
		if _, err := sc.Store.DB.Exec(`DELETE FROM files WHERE source_id=? AND path=?`, fr.SourceID, fr.Path); err != nil {
			log.Printf("organize: drop non-episode file path=%q failed: %v", fr.Path, err)
		}
	}
	if len(skippedFiles) > 0 && prog != nil {
		prog.AddEvent("warn", fmt.Sprintf("跳过 %d 个无集号文件(剧集库只收剧集)", len(skippedFiles)))
	}

	if err := sc.cleanup(lib, imported, current, walked, partial, listings, scopes...); err != nil {
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

// cleanup deletes the files a library no longer holds, and the items left empty.
//
// A file is identified by the source it came from *and* its path: a library may
// scan several sources, and the same path in two of them is two different files.
//
// current is the set of sources this library scans now, so a file whose source
// is no longer one of them is deleted: the library stopped scanning there, and
// those rows belong to nobody.
//
// For a source the library still scans, the walk's own directory listings are
// the evidence. A listing is proof about exactly one directory:
//
//   - the file's own directory was read this scan and the file is not among the
//     files stored, so the row is stale (the file is gone, was filtered out by
//     size, or is no longer a video);
//   - a directory further up was read and the folder leading to the file is no
//     longer in it, so the whole subtree is gone and its rows go with it;
//   - no directory above the file was read (the drive was asleep, the source
//     failed, the API rate-limited the listing), so nothing is deleted. Not
//     being able to read a source is not evidence that its files are gone, and
//     deleting them would take the rows and the watch progress with them.
//
// A partial source (bilibili's feed) is different: its root listing is a sample,
// so a series missing from it has not necessarily been deleted. There, only the
// first rule applies — plus a legacy path whose episode is now listed elsewhere
// (the source restructured its tree, and the same [epN] moved to a new path).
//
// The previous version only cleaned a source whose *entire* walk succeeded,
// which on a bilibili library (hundreds of calls, one rate-limited season) is
// almost never: every path the tree had before a restructure stayed in the
// database, and the same series appeared as both the old flat entries and the
// new series/season tree.
func (sc *Scanner) cleanup(lib *models.Library, files []fileRec, current, walked, partial map[int64]bool, listings map[string][]source.Entry, scopes ...*scanScope) error {
	seen := map[string]bool{}
	// Episode ids the walk found, for a partial source: a stale path that names
	// the same [epN] is the old location of a file that now lives elsewhere.
	seenEp := map[string]bool{}
	for _, fr := range files {
		seen[fileSourceKey(fr.SourceID, fr.Path)] = true
		if tag := fileEpisodeTag(baseName(fr.Path)); tag != "" {
			seenEp[fileSourceKey(fr.SourceID, tag)] = true
		}
	}
	rows, err := sc.Store.DB.Query(`SELECT f.id, f.source_id, f.path FROM files f JOIN items i ON i.id = f.item_id WHERE i.library_id = ?`, lib.ID)
	if err != nil {
		return err
	}
	var gone []int64
	var scope *scanScope
	if len(scopes) > 0 {
		scope = scopes[0]
	}
	for rows.Next() {
		var id, srcID int64
		var p string
		if err := rows.Scan(&id, &srcID, &p); err != nil {
			rows.Close()
			return err
		}
		// Unseen paths outside this subtree say nothing about deletion. STRM
		// rows store only their URL, so their original directory is unknown and
		// unseen stream URLs must also be retained by a path scan.
		if scope != nil && (srcID != scope.SourceID || !pathWithin(p, scope.Path)) {
			continue
		}
		if !current[srcID] {
			gone = append(gone, id)
			continue
		}
		if seen[fileSourceKey(srcID, p)] {
			continue
		}
		if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
			// A .strm row stores the URL it points at, not the path the walk
			// read, so a listing cannot say whether it is gone. Only a source
			// read to the end can.
			if walked[srcID] {
				gone = append(gone, id)
			}
			continue
		}
		if partial[srcID] {
			// A feed's root is not an inventory: only the file's own directory
			// can prove it gone, or the same episode having moved.
			if fileDirListed(srcID, p, listings) {
				gone = append(gone, id)
				continue
			}
			if tag := fileEpisodeTag(baseName(p)); tag != "" && seenEp[fileSourceKey(srcID, tag)] {
				gone = append(gone, id)
			}
			continue
		}
		if fileGoneFromListings(srcID, p, listings) {
			gone = append(gone, id)
		}
	}
	rows.Close()
	var removedItems []int64
	for _, id := range gone {
		if scope != nil {
			f, err := sc.Store.FileByID(id)
			if err != nil {
				return err
			}
			removedItems = append(removedItems, f.ItemID)
		}
		if _, err := sc.Store.DB.Exec(`DELETE FROM files WHERE id=?`, id); err != nil {
			return err
		}
	}
	if scope != nil {
		return sc.pruneScanItems(lib.ID, removedItems)
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

// fileDirListed reports whether this scan read the directory that holds path.
// When it did, a file that is not among the walk's files is gone from it.
func fileDirListed(srcID int64, path string, listings map[string][]source.Entry) bool {
	dir := parentDir(path)
	if dir == "" {
		dir = "/"
	}
	_, ok := listings[fmt.Sprintf("%d|%s", srcID, dir)]
	return ok
}

// reFileEpisodeTag matches the episode marker a source may keep in a file name
// ("… [ep395228].mp4"). It is what lets a restructured tree recognise that a
// path which disappeared is the same episode as one that is now listed.
var reFileEpisodeTag = regexp.MustCompile(`(?i)\[ep(\d+)\](?:\.[a-z0-9]+)?$`)

func fileEpisodeTag(name string) string {
	if m := reFileEpisodeTag.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return ""
}

// fileGoneFromListings reports whether a directory this scan read proves that
// path no longer exists. The walk stores one listing per directory it managed
// to read, keyed by source and path; a failed (or unread) directory has no entry
// and proves nothing.
func fileGoneFromListings(srcID int64, path string, listings map[string][]source.Entry) bool {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) < 2 {
		return false
	}
	for i := len(segs) - 1; i >= 0; i-- {
		dir := "/"
		if i > 0 {
			dir = "/" + strings.Join(segs[:i], "/")
		}
		ents, ok := listings[fmt.Sprintf("%d|%s", srcID, dir)]
		if !ok {
			continue
		}
		if i == len(segs)-1 {
			// The file's own directory was read and the file was not stored.
			return true
		}
		for _, e := range ents {
			if e.Name == segs[i] {
				// The folder leading down to the file is still there; whatever
				// is missing is deeper than what this scan managed to read.
				return false
			}
		}
		return true
	}
	return false
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

// displayBaseName is baseName without the video extension. It is the last-resort
// name for a file the parser could not identify: "…[1080p][JPSC].mp4" is a bad
// name for a show, but it is not made better by ending in ".mp4".
func displayBaseName(path string) string {
	b := baseName(path)
	if i := strings.LastIndex(b, "."); i > 0 {
		return b[:i]
	}
	return b
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

// reBiliSeasonDir matches the id a bilibili season folder carries:
// "第二季 [season 46085]", "TV [season 3398]", "SP [season 4026]". The bracket
// is what says the folder is a season rather than a show. A single-season
// series has exactly one such folder and bilibili names it "TV"; without this
// marker every one of those series was planned as a show named "TV", and they
// all merged into one mega-show.
var reBiliSeasonDir = regexp.MustCompile(`(?i)\s*\[season\s+\d+\]\s*$`)

// biliSeasonTitle returns a bilibili season folder's own title without its id
// bracket, and whether the folder is one at all.
func biliSeasonTitle(name string) (string, bool) {
	n := strings.TrimSpace(name)
	if !reBiliSeasonDir.MatchString(n) {
		return "", false
	}
	return strings.TrimSpace(reBiliSeasonDir.ReplaceAllString(n, "")), true
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
	// How many segments the library root itself takes. A source that *is* the
	// library root ("/", as the bilibili tree is) has none, so the first segment
	// is already a show. Counting from 1 here is what made a show sitting
	// directly under the root look like a directory inside a season: the series
	// folder /装甲重拳 [series 1]/第二季 [season 2]/… resolved to the *season*
	// folder, and a one-season series never got a show of its own.
	rootDepth := 0
	if lib != "" && lib != "/" {
		rootDepth = len(strings.Split(strings.Trim(lib, "/"), "/"))
	}

	for i := len(segs) - 1; i >= 1; i-- {
		if IsContainerDir(segs[i]) {
			continue
		}
		_, isSeason := ParseSeasonDir(segs[i])
		if !isSeason {
			_, isSeason = seasonMarkerDir(segs[i])
		}
		_, _, isSpecial := SpecialSeasonDir(segs[i])
		_, isBili := biliSeasonTitle(segs[i])
		if !isSeason && !isSpecial && !isNumericDir(segs[i]) && !isBili {
			continue
		}
		j := i - 1
		for j >= rootDepth && (isTechDir(segs[j]) || isNumericDir(segs[j])) {
			j--
		}
		if j >= rootDepth {
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
		// A bilibili season folder carries its id in a bracket. Its title is
		// what the card should say, and its shape decides the slot: "TV" is the
		// ordinary first season, "第二季" is season 2, "SP" stays a named
		// special, and "游郭篇" is a named arc.
		if title, ok := biliSeasonTitle(segs[i]); ok {
			seriesTitle := DisplayName(baseName(showDir))
			if strings.EqualFold(strings.TrimSpace(title), "tv") ||
				(seriesTitle != "" && NormalizeKey(title) == NormalizeKey(seriesTitle)) {
				return 1, "第 1 季", true
			}
			if n, ok := ParseSeasonDir(title); ok {
				return n, fmt.Sprintf("第 %d 季", n), true
			}
			if strings.EqualFold(strings.TrimSpace(title), "sp") || strings.EqualFold(strings.TrimSpace(title), "sps") {
				return 0, title, true
			}
			if disp, num, ok := SpecialSeasonDir(title); ok {
				return num, disp, true
			}
			return 0, title, true
		}
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
		if n, ok := seasonMarkerDir(segs[i]); ok {
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

// reSeasonPartLabel matches the note a source uses to split one numbered season
// into broadcast halves: 第二季（前半）/ 第二季（后半）, "Season 2 Part 2", 前篇 /
// 後編. It is anchored at the end so a name that merely contains 前/后
// (最后的…, 前后篇) is left alone.
var reSeasonPartLabel = regexp.MustCompile(`(?i)[\s·•\-—_（(【\[]*(?:前半|后半|前篇|后篇|前編|後編|上篇|下篇|part\s*[12]|part\s*(?:i|ii))(?:[\s·•\-—_）)】\]]*)$`)

// seasonBaseLabel removes a trailing "前半/后半" marker from a season name:
// "第二季（前半）" and "第二季（后半）" both become "第二季".
func seasonBaseLabel(name string) string {
	s := strings.TrimSpace(name)
	s = reSeasonPartLabel.ReplaceAllString(s, "")
	return strings.TrimSpace(strings.TrimRight(s, "·•-—_（(【["))
}

// seasonFamily is what makes two labels the same season: the number when the
// label carries one ("第二季（前半）", "第 2 季" → 2), the name when it does not
// ("游郭篇" → 游郭篇).
func seasonFamily(name string) string {
	if n, ok := ParseSeasonDir(seasonBaseLabel(name)); ok {
		return fmt.Sprintf("第%d季", n)
	}
	return NormalizeKey(seasonBaseLabel(name))
}

// mergeSeasonLabels returns the label two parts of one season should share, or
// the existing label when they are not parts of the same season.
func mergeSeasonLabels(existing, incoming string) string {
	if existing == incoming {
		return existing
	}
	if fam := seasonFamily(existing); fam != "" && fam == seasonFamily(incoming) {
		if base := seasonBaseLabel(existing); base != "" {
			return base
		}
	}
	return existing
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

// sourceLabel reads the name the source itself gave an entry, for sources that
// name what they list.
//
// bilibili writes a file as "<series> <its own label>", where the label is the
// episode's title ("第3话 结婚") or, for a section entry, the section and the
// entry's name ("PV&其他 第3话预告 新国王"). That is the title the show is
// published under, it is right for *this* season, and it beats asking a provider
// to guess which episode of which season a row is — the guess is how a second
// season ended up carrying the first season's episode titles.
//
// Empty for every other source: a local release name is not an episode title.
func sourceLabel(fr fileRec, showDir string) string {
	if fr.SrcType != "bilibili" {
		return ""
	}
	stem := baseName(fr.Path)
	if i := strings.LastIndex(stem, "."); i >= 0 {
		stem = stem[:i]
	}
	label := stem
	if showDir != "" {
		series := DisplayName(baseName(showDir))
		if series != "" && strings.HasPrefix(label, series) {
			label = label[len(series):]
		}
	}
	label = reExtraTag.ReplaceAllString(label, " ")
	label = reFileEpisodeTag.ReplaceAllString(label, " ")
	label = strings.Join(strings.Fields(label), " ")
	// Nothing but the series name left: there is no label to prefer.
	if label == "" || NormalizeKey(label) == NormalizeKey(stem) {
		return ""
	}
	return label
}
