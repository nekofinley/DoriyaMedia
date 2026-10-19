package scan

import (
	"strings"
	"testing"

	"doriyamedia/internal/models"
)

// bilibili keeps 特别篇 / PV / 预告 in sections beside the episodes, and names the
// entries the way a viewer would: "第3话预告" is the preview of episode 3. Its
// name contains "第3话", so reading the name alone puts it on episode 3 — where it
// became a second version of the episode, and the preview is what played when the
// episode was picked. The source states which section an entry belongs to, and
// that statement is read first.
func TestSectionEntryIsNotAnEpisodeVersion(t *testing.T) {
	const (
		dir  = "/热门番剧/国王排名 [series 39462]/TV [season 39462]"
		main = dir + "/国王排名 第3话 新国王 [ep424607].mp4"
		pv   = dir + "/国王排名 PV&其他 第3话预告 新国王 [sp#2#PV&其他] [ep427417].mp4"
	)

	ep := ParseName(baseName(main))
	if ep.Extra || ep.Episode != 3 {
		t.Fatalf("the episode parsed as extra=%v ep=%d", ep.Extra, ep.Episode)
	}
	extra := ParseName(baseName(pv))
	if !extra.Extra || extra.ExtraAt != 2 || extra.ExtraName != "PV&其他" {
		t.Fatalf("the preview parsed as extra=%v at=%d name=%q", extra.Extra, extra.ExtraAt, extra.ExtraName)
	}
	if extra.Episode != extra.ExtraAt {
		t.Fatalf("an extra's order must not be mistaken for an episode number")
	}

	// The label the card shows is the source's own wording for the entry.
	fr := fileRec{Path: pv, Dir: dir, SourceID: 1, SrcType: "bilibili"}
	if got := sourceLabel(fr, showDirFor("/热门番剧", dir)); got != "PV&其他 第3话预告 新国王" {
		t.Fatalf("sourceLabel = %q", got)
	}
	frMain := fileRec{Path: main, Dir: dir, SourceID: 1, SrcType: "bilibili"}
	if got := sourceLabel(frMain, showDirFor("/热门番剧", dir)); got != "第3话 新国王" {
		t.Fatalf("sourceLabel(main) = %q", got)
	}
	// A local release keeps the placeholder name: its file name is not a title.
	frLocal := fileRec{Path: main, Dir: dir, SourceID: 1, SrcType: "local"}
	if got := sourceLabel(frLocal, showDirFor("/热门番剧", dir)); got != "" {
		t.Fatalf("sourceLabel(local) = %q, want no label", got)
	}
}

// The organizer's side of the same rule: the two files end up on two cards, and
// neither card holds a second version of the other's episode.
func TestOrganizerGivesSectionsTheirOwnCard(t *testing.T) {
	store, lib, sourceID := newScanTestLibrary(t)
	const dir = "/热门番剧/国王排名 [series 39462]/TV [season 39462]"
	files := []fileRec{
		{Path: dir + "/国王排名 第3话 新国王 [ep424607].mp4", Dir: dir, SourceID: sourceID, Root: "/热门番剧", SrcType: "bilibili"},
		{Path: dir + "/国王排名 PV&其他 第3话预告 新国王 [sp#2#PV&其他] [ep427417].mp4", Dir: dir, SourceID: sourceID, Root: "/热门番剧", SrcType: "bilibili"},
	}
	sc := &Scanner{Store: store}
	prog := models.NewScanProgress(lib.ID)
	if err := sc.organizeSeries(lib, files, prog,
		map[int64]bool{sourceID: true}, map[int64]bool{sourceID: true}, map[int64]bool{sourceID: true}, nil); err != nil {
		t.Fatalf("organizeSeries: %v", err)
	}

	shows, _, err := store.ItemsByLibrary(lib.ID, "show", "", "", false, 0, 100)
	if err != nil || len(shows) != 1 {
		t.Fatalf("shows = %d, %v", len(shows), err)
	}
	seasons, _, err := store.ItemsByLibrary(lib.ID, "season", "", "", false, 0, 100)
	if err != nil {
		t.Fatalf("seasons: %v", err)
	}
	if len(seasons) != 2 {
		for _, s := range seasons {
			t.Logf("season %d %q", s.Season, s.Name)
		}
		t.Fatalf("got %d seasons, want the episode season and the PV section", len(seasons))
	}

	seen := map[string]string{}
	for _, season := range seasons {
		kids, err := store.ItemChildren(season.ID)
		if err != nil {
			t.Fatalf("children of %q: %v", season.Name, err)
		}
		if len(kids) != 1 {
			t.Fatalf("season %q holds %d episodes, want 1", season.Name, len(kids))
		}
		fs, err := store.FilesByItem(kids[0].ID)
		if err != nil || len(fs) != 1 {
			t.Fatalf("episode %q holds %d files, want 1 (%v)", kids[0].Name, len(fs), err)
		}
		seen[season.Name] = kids[0].Name
	}

	if got := seen["第 1 季"]; got != "第3话 新国王" {
		t.Fatalf("episode card = %q, want bilibili's own label", got)
	}
	pvName, ok := seen["PV&其他"]
	if !ok {
		t.Fatalf("the PV section has no card of its own: %v", seen)
	}
	if !strings.Contains(pvName, "第3话预告") {
		t.Fatalf("PV card = %q, want bilibili's own label for it", pvName)
	}
}

// A second scan has to repair names a provider guessed by episode number: the
// source's own label wins, so a season that had been given another season's
// titles is corrected rather than left alone because it "has a real name".
func TestRescanTakesTheSourceLabelOverAGuessedName(t *testing.T) {
	store, lib, sourceID := newScanTestLibrary(t)
	const dir = "/热门番剧/国王排名 [series 39462]/TV [season 39462]"
	files := []fileRec{
		{Path: dir + "/国王排名 第3话 新国王 [ep424607].mp4", Dir: dir, SourceID: sourceID, Root: "/热门番剧", SrcType: "bilibili"},
	}
	sc := &Scanner{Store: store}
	run := func() {
		if err := sc.organizeSeries(lib, files, models.NewScanProgress(lib.ID),
			map[int64]bool{sourceID: true}, map[int64]bool{sourceID: true}, map[int64]bool{sourceID: true}, nil); err != nil {
			t.Fatalf("organizeSeries: %v", err)
		}
	}
	run()

	// Stand in for an enrichment run that mistook this for another season's
	// episode 3.
	seasons, _, err := store.ItemsByLibrary(lib.ID, "season", "", "", false, 0, 100)
	if err != nil || len(seasons) != 1 {
		t.Fatalf("seasons = %d, %v", len(seasons), err)
	}
	kids, err := store.ItemChildren(seasons[0].ID)
	if err != nil || len(kids) != 1 {
		t.Fatalf("episodes = %d, %v", len(kids), err)
	}
	kids[0].Name = "结婚"
	if err := store.UpdateItemMeta(&kids[0]); err != nil {
		t.Fatalf("update: %v", err)
	}

	run()
	after, err := store.ItemByID(kids[0].ID)
	if err != nil {
		t.Fatalf("episode: %v", err)
	}
	if after.Name != "第3话 新国王" {
		t.Fatalf("rescan kept %q, want the source's own label", after.Name)
	}
}
