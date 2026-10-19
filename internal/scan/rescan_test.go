package scan

import (
	"fmt"
	"path/filepath"
	"testing"

	"doriyamedia/internal/db"
	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

// A source whose listing failed somewhere is not a source whose files are gone.
// Cleanup used to treat it as one: any walk error turned the whole library's
// cleanup off, so a restructured source kept every old path beside the new one
// forever. These tests pin the per-directory rule that replaced it.
func TestCleanupOnlyDeletesWhatTheListingsProveGone(t *testing.T) {
	store, lib, sourceID := newScanTestLibrary(t)

	oldShow := addShow(t, store, lib.ID, "OldShow")
	oldSeason := addSeason(t, store, oldShow, "第 1 季", 1)
	oldEp := addEpisode(t, store, oldSeason, 1, 1, "/OldShow/ep1.mp4", sourceID)

	keepShow := addShow(t, store, lib.ID, "KeepShow")
	keepSeason := addSeason(t, store, keepShow, "第 1 季", 1)
	keepEp := addEpisode(t, store, keepSeason, 1, 1, "/KeepShow/ep1.mp4", sourceID)

	failShow := addShow(t, store, lib.ID, "FailShow")
	failSeason := addSeason(t, store, failShow, "第 1 季", 1)
	failEp := addEpisode(t, store, failSeason, 1, 1, "/FailShow/ep1.mp4", sourceID)

	rootKey := fmt.Sprintf("%d|/", sourceID)
	listings := map[string][]source.Entry{
		rootKey: {
			{Name: "KeepShow", Path: "/KeepShow", IsDir: true},
			{Name: "FailShow", Path: "/FailShow", IsDir: true},
			// OldShow is gone from the source root: its whole subtree is gone.
		},
		fmt.Sprintf("%d|/KeepShow", sourceID): {
			{Name: "ep1.mp4", Path: "/KeepShow/ep1.mp4"},
		},
		// /FailShow was not read this scan: nothing under it may be deleted.
	}
	sc := &Scanner{Store: store}
	// Only KeepShow is in the walk's result; old and fail are rows the walk did
	// not confirm (and must be judged by the listings, not by "not seen").
	files := []fileRec{{Path: "/KeepShow/ep1.mp4", SourceID: sourceID}}
	err := sc.cleanup(lib, files,
		map[int64]bool{sourceID: true},
		map[int64]bool{}, // the walk had an error somewhere
		map[int64]bool{},
		listings)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	if _, err := store.ItemByID(oldEp); err == nil {
		t.Fatalf("episode under a removed show survived cleanup")
	}
	if _, err := store.ItemByID(oldSeason); err == nil {
		t.Fatalf("season under a removed show survived cleanup")
	}
	if _, err := store.ItemByID(oldShow); err == nil {
		t.Fatalf("show removed from the source survived cleanup")
	}
	for _, id := range []int64{keepShow, keepSeason, keepEp, failShow, failSeason, failEp} {
		if _, err := store.ItemByID(id); err != nil {
			t.Fatalf("item %d was deleted although its directory was not proven gone: %v", id, err)
		}
	}
}

// A source that could not be read at all proves nothing, even if the walk
// returned no files: every row has to stay.
func TestCleanupKeepsEverythingWhenNoDirectoryWasRead(t *testing.T) {
	store, lib, sourceID := newScanTestLibrary(t)
	show := addShow(t, store, lib.ID, "Unreadable")
	season := addSeason(t, store, show, "第 1 季", 1)
	ep := addEpisode(t, store, season, 1, 1, "/Unreadable/ep1.mp4", sourceID)

	sc := &Scanner{Store: store}
	if err := sc.cleanup(lib, nil, map[int64]bool{sourceID: true}, map[int64]bool{}, map[int64]bool{}, nil); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := store.ItemByID(ep); err != nil {
		t.Fatalf("episode was deleted although its source was never read")
	}
}

// A partial source (bilibili's feed) must not treat a series dropping out of
// its root listing as deletion — that removed 102 shows in one live scan. It
// still removes a legacy path when the same episode is now listed elsewhere,
// and a file whose own directory was read and no longer holds it.
func TestCleanupOnPartialSource(t *testing.T) {
	store, lib, sourceID := newScanTestLibrary(t)

	// A show that is simply not in this scan's feed any more.
	keptShow := addShow(t, store, lib.ID, "NotInFeed")
	keptSeason := addSeason(t, store, keptShow, "第 1 季", 1)
	keptEp := addEpisode(t, store, keptSeason, 1, 1, "/NotInFeed/ep1.mp4", sourceID)

	// The same episode at its old flat path and at its new series path.
	newShow := addShow(t, store, lib.ID, "NewTree")
	newSeason := addSeason(t, store, newShow, "第 1 季", 1)
	newEp := addEpisode(t, store, newSeason, 1, 1,
		"/NewTree [series 1]/第一季 [season 1]/Show 第1话 [ep900].mp4", sourceID)
	legacyShow := addShow(t, store, lib.ID, "OldFlat")
	legacySeason := addSeason(t, store, legacyShow, "第 1 季", 1)
	legacyEp := addEpisode(t, store, legacySeason, 1, 1,
		"/OldFlat [season 1]/Show 第1话 [ep900].mp4", sourceID)

	// A file whose own directory was read, but which is gone from it.
	removedShow := addShow(t, store, lib.ID, "RemovedFile")
	removedSeason := addSeason(t, store, removedShow, "第 1 季", 1)
	removedEp := addEpisode(t, store, removedSeason, 1, 1, "/RemovedFile/gone.mp4", sourceID)

	listings := map[string][]source.Entry{
		fmt.Sprintf("%d|/", sourceID): {
			{Name: "NewTree [series 1]", Path: "/NewTree [series 1]", IsDir: true},
			{Name: "RemovedFile", Path: "/RemovedFile", IsDir: true},
			// NotInFeed and OldFlat are absent: for a partial source that is
			// not proof they are gone.
		},
		fmt.Sprintf("%d|/RemovedFile", sourceID): {
			{Name: "other.mp4", Path: "/RemovedFile/other.mp4"},
		},
	}
	files := []fileRec{
		newRec("/NewTree [series 1]/第一季 [season 1]/Show 第1话 [ep900].mp4", sourceID),
	}
	sc := &Scanner{Store: store}
	err := sc.cleanup(lib, files,
		map[int64]bool{sourceID: true},
		map[int64]bool{},
		map[int64]bool{sourceID: true}, // bilibili: the root is a feed sample
		listings)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	if _, err := store.ItemByID(keptEp); err != nil {
		t.Fatalf("episode of a show that left the feed was deleted")
	}
	if _, err := store.ItemByID(newEp); err != nil {
		t.Fatalf("the new tree's episode was deleted")
	}
	if _, err := store.ItemByID(legacyEp); err == nil {
		t.Fatalf("legacy path with the same episode still listed survived cleanup")
	}
	if _, err := store.ItemByID(legacyShow); err == nil {
		t.Fatalf("show left with no episodes survived cleanup")
	}
	if _, err := store.ItemByID(removedEp); err == nil {
		t.Fatalf("file missing from its own listed directory survived cleanup")
	}
	if _, err := store.ItemByID(removedShow); err == nil {
		t.Fatalf("show left with no episodes survived cleanup")
	}
}

// The rescan that motivated this: a bilibili series is reorganized from flat
// season folders into one series directory, the metadata pass has already
// renamed the show ("装甲重拳 MEGALOBOX" → "MEGALO BOX"), and a second scan must
//
//   - keep the renamed show instead of creating a fresh one for the folder name,
//   - put 第二季（前半） and 第二季（后半） on one card with the episode numbers
//     they already have (14..25 continue after 1..13),
//   - delete the old flat tree and its show.
func TestRescanKeepsRenamedShowAndMergesSeasonParts(t *testing.T) {
	store, lib, sourceID := newScanTestLibrary(t)

	renamed := addShow(t, store, lib.ID, "MEGALO BOX")
	firstSeason := addSeason(t, store, renamed, "第一季", 1)
	addEpisode(t, store, firstSeason, 1, 1, seriesPath("第一季", 1), sourceID)
	addEpisode(t, store, firstSeason, 1, 2, seriesPath("第一季", 2), sourceID)
	secondSeason := addSeason(t, store, renamed, "第二季（前半）", 2)
	addEpisode(t, store, secondSeason, 2, 1, seriesPath("第二季（前半）", 1), sourceID)
	addEpisode(t, store, secondSeason, 2, 2, seriesPath("第二季（前半）", 2), sourceID)

	stale := addShow(t, store, lib.ID, "装甲重拳 MEGALOBOX 第二季")
	staleSeason := addSeason(t, store, stale, "第 1 季", 1)
	// The same four episodes the new tree holds, at their old flat paths.
	for i := 1; i <= 4; i++ {
		addEpisode(t, store, staleSeason, 1, i,
			fmt.Sprintf("/装甲重拳_MEGALOBOX 第二季 [38234]/装甲重拳_MEGALOBOX 第%d话 [ep%d].mp4", i, i), sourceID)
	}

	files := []fileRec{
		newRec(seriesPath("第一季", 1), sourceID),
		newRec(seriesPath("第一季", 2), sourceID),
		newRec(seriesPath("第二季（前半）", 1), sourceID),
		newRec(seriesPath("第二季（前半）", 2), sourceID),
		newRec(seriesPath("第二季（后半）", 3), sourceID),
		newRec(seriesPath("第二季（后半）", 4), sourceID),
	}
	listings := seriesListings(sourceID, files)
	sc := &Scanner{Store: store}
	prog := models.NewScanProgress(lib.ID)
	current := map[int64]bool{sourceID: true}

	for pass := 1; pass <= 2; pass++ {
		if err := sc.organizeSeries(lib, files, prog, current, map[int64]bool{}, map[int64]bool{sourceID: true}, listings); err != nil {
			t.Fatalf("pass %d: organizeSeries: %v", pass, err)
		}
	}

	shows, _, err := store.ItemsByLibrary(lib.ID, "show", "", "", false, 0, 100)
	if err != nil {
		t.Fatalf("list shows: %v", err)
	}
	if len(shows) != 1 {
		t.Fatalf("show count = %d (%v), want 1: the rescan split one series again", len(shows), showNames(shows))
	}
	if shows[0].ID != renamed {
		t.Fatalf("show = %q (id %d), want the renamed show %q (id %d)", shows[0].Name, shows[0].ID, "MEGALO BOX", renamed)
	}
	if _, err := store.ItemByID(stale); err == nil {
		t.Fatalf("the old flat show survived the rescan")
	}

	seasons, _ := store.ItemChildren(renamed)
	if len(seasons) != 2 {
		for _, s := range seasons {
			eps, _ := store.ItemChildren(s.ID)
			t.Logf("season %d %q num=%d eps=%d", s.ID, s.Name, s.Season, len(eps))
		}
		t.Fatalf("season count = %d, want 2 (第一季, 第二季)", len(seasons))
	}
	second := findSeason(t, seasons, 2)
	if second.Name != "第二季" {
		t.Errorf("second season label = %q, want 第二季 (both halves on one card)", second.Name)
	}
	eps, _ := store.ItemChildren(second.ID)
	if len(eps) != 4 {
		t.Fatalf("second season episode count = %d, want 4", len(eps))
	}
	for i, want := range []int{1, 2, 3, 4} {
		if eps[i].Episode != want {
			t.Errorf("episode %d = %d, want %d", i, eps[i].Episode, want)
		}
	}
	first := findSeason(t, seasons, 1)
	if first.Name != "第一季" {
		t.Errorf("first season label = %q, want 第一季", first.Name)
	}
}

func newScanTestLibrary(t *testing.T) (*db.Store, *models.Library, int64) {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "scan.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = store.DB.Close() })
	src := &models.Source{Name: "test", Type: "local", Config: `{"root":"/tmp"}`}
	if err := store.CreateSource(src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	lib := &models.Library{Name: "test", Type: "series", SourceID: src.ID, Path: "/", Providers: `[]`}
	if err := store.CreateLibrary(lib); err != nil {
		t.Fatalf("create library: %v", err)
	}
	return store, lib, src.ID
}

func addShow(t *testing.T, store *db.Store, libID int64, name string) int64 {
	t.Helper()
	it := &models.Item{LibraryID: libID, Type: "show", Name: name}
	if err := store.InsertItem(it); err != nil {
		t.Fatalf("insert show %q: %v", name, err)
	}
	return it.ID
}

func addSeason(t *testing.T, store *db.Store, showID int64, name string, num int) int64 {
	t.Helper()
	pid := showID
	it := &models.Item{LibraryID: 0, Type: "season", ParentID: &pid, Name: name, Season: num}
	// The library id is inherited from the parent by every reader; fill it to
	// satisfy the schema.
	if show, err := store.ItemByID(showID); err == nil {
		it.LibraryID = show.LibraryID
	}
	if err := store.InsertItem(it); err != nil {
		t.Fatalf("insert season %q: %v", name, err)
	}
	return it.ID
}

func addEpisode(t *testing.T, store *db.Store, seasonID int64, season, ep int, path string, sourceID int64) int64 {
	t.Helper()
	pid := seasonID
	it := &models.Item{Type: "episode", ParentID: &pid, Name: fmt.Sprintf("第 %d 集", ep), Season: season, Episode: ep}
	if s, err := store.ItemByID(seasonID); err == nil {
		it.LibraryID = s.LibraryID
	}
	if err := store.InsertItem(it); err != nil {
		t.Fatalf("insert episode %d: %v", ep, err)
	}
	if _, err := store.UpsertFile(&models.MediaFile{ItemID: it.ID, SourceID: sourceID, Path: path}); err != nil {
		t.Fatalf("insert file %q: %v", path, err)
	}
	return it.ID
}

func seriesPath(seasonDir string, ep int) string {
	return fmt.Sprintf("/MEGALO BOX [series 38001]/%s [season 3%d]/MEGALO BOX 第%d话 [ep%d].mp4",
		seasonDir, len(seasonDir), ep, ep)
}

// newRec builds the record the walk hands the organizer: the file, the
// directory it was listed in, and the library root it was found under.
func newRec(path string, sourceID int64) fileRec {
	return fileRec{Path: path, Dir: parentDir(path), Root: "/", SourceID: sourceID}
}

func seriesListings(sourceID int64, files []fileRec) map[string][]source.Entry {
	out := map[string][]source.Entry{
		fmt.Sprintf("%d|/", sourceID): {
			{Name: "MEGALO BOX [series 38001]", Path: "/MEGALO BOX [series 38001]", IsDir: true},
		},
	}
	seenDir := map[string]bool{}
	for _, fr := range files {
		dir := parentDir(fr.Path)
		if !seenDir[dir] {
			seenDir[dir] = true
			out[fmt.Sprintf("%d|%s", sourceID, dir)] = nil
		}
		out[fmt.Sprintf("%d|%s", sourceID, dir)] = append(out[fmt.Sprintf("%d|%s", sourceID, dir)],
			source.Entry{Name: baseName(fr.Path), Path: fr.Path})
		if p := parentDir(dir); p != "" && !seenDir[p] {
			seenDir[p] = true
			out[fmt.Sprintf("%d|%s", sourceID, p)] = []source.Entry{
				{Name: baseName(dir), Path: dir, IsDir: true},
			}
		}
	}
	return out
}

func findSeason(t *testing.T, seasons []models.Item, num int) models.Item {
	t.Helper()
	for _, s := range seasons {
		if s.Season == num {
			return s
		}
	}
	t.Fatalf("no season with number %d in %+v", num, seasons)
	return models.Item{}
}

func showNames(items []models.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, fmt.Sprintf("%d:%s", it.ID, it.Name))
	}
	return out
}
