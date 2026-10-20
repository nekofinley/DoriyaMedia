package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"doriyamedia/internal/models"
)

// The badge in the UI reads items.source_type. It has to be the type of every
// file behind the item, and it has to be empty (no badge) when a show or season
// mixes files from different kinds of source — a single wrong badge is worse
// than none.
func TestRefreshItemSourceTypes(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "source-type.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.DB.Close()

	bili := &models.Source{Name: "B站", Type: "bilibili", Config: "{}"}
	local := &models.Source{Name: "本地", Type: "local", Config: "{}"}
	for _, s := range []*models.Source{bili, local} {
		if err := store.CreateSource(s); err != nil {
			t.Fatalf("create source: %v", err)
		}
	}
	lib := &models.Library{Name: "番剧", Type: "series", SourceID: bili.ID, Path: "/", Providers: "[]"}
	if err := store.CreateLibrary(lib); err != nil {
		t.Fatalf("create library: %v", err)
	}

	// show → season → episode, the shape the scanner writes.
	show := &models.Item{LibraryID: lib.ID, Type: "show", Name: "全部来自B站"}
	if err := store.InsertItem(show); err != nil {
		t.Fatalf("insert show: %v", err)
	}
	season := &models.Item{LibraryID: lib.ID, Type: "season", ParentID: &show.ID, Name: "第一季", Season: 1}
	if err := store.InsertItem(season); err != nil {
		t.Fatalf("insert season: %v", err)
	}
	ep1 := &models.Item{LibraryID: lib.ID, Type: "episode", ParentID: &season.ID, Name: "第 1 集", Season: 1, Episode: 1}
	ep2 := &models.Item{LibraryID: lib.ID, Type: "episode", ParentID: &season.ID, Name: "第 2 集", Season: 1, Episode: 2}
	for _, ep := range []*models.Item{ep1, ep2} {
		if err := store.InsertItem(ep); err != nil {
			t.Fatalf("insert episode: %v", err)
		}
	}
	for i, ep := range []*models.Item{ep1, ep2} {
		if _, err := store.UpsertFile(&models.MediaFile{
			ItemID: ep.ID, SourceID: bili.ID,
			Path: "/bili/" + string(rune('a'+i)) + ".mp4", Size: 1, MTime: 1,
		}); err != nil {
			t.Fatalf("upsert bilibili file: %v", err)
		}
	}

	if err := store.RefreshItemSourceTypes(lib.ID); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	for _, it := range []*models.Item{show, season, ep1, ep2} {
		got, err := store.ItemByID(it.ID)
		if err != nil {
			t.Fatalf("reload %s: %v", it.Type, err)
		}
		if got.SourceType != "bilibili" {
			t.Errorf("%s %q source_type = %q, want bilibili", it.Type, it.Name, got.SourceType)
		}
	}

	// A second show whose two episodes come from two different kinds of source
	// has no single true badge: the show and season stay blank, the episodes do
	// not.
	mixedShow := &models.Item{LibraryID: lib.ID, Type: "show", Name: "混合来源"}
	if err := store.InsertItem(mixedShow); err != nil {
		t.Fatalf("insert mixed show: %v", err)
	}
	mixedSeason := &models.Item{LibraryID: lib.ID, Type: "season", ParentID: &mixedShow.ID, Name: "第一季", Season: 1}
	if err := store.InsertItem(mixedSeason); err != nil {
		t.Fatalf("insert mixed season: %v", err)
	}
	mixedEp1 := &models.Item{LibraryID: lib.ID, Type: "episode", ParentID: &mixedSeason.ID, Name: "第 1 集", Season: 1, Episode: 1}
	mixedEp2 := &models.Item{LibraryID: lib.ID, Type: "episode", ParentID: &mixedSeason.ID, Name: "第 2 集", Season: 1, Episode: 2}
	for _, ep := range []*models.Item{mixedEp1, mixedEp2} {
		if err := store.InsertItem(ep); err != nil {
			t.Fatalf("insert mixed episode: %v", err)
		}
	}
	if _, err := store.UpsertFile(&models.MediaFile{ItemID: mixedEp1.ID, SourceID: bili.ID, Path: "/mixed1.mp4", Size: 1, MTime: 1}); err != nil {
		t.Fatalf("upsert mixed file: %v", err)
	}
	if _, err := store.UpsertFile(&models.MediaFile{ItemID: mixedEp2.ID, SourceID: local.ID, Path: "/mixed2.mp4", Size: 1, MTime: 1}); err != nil {
		t.Fatalf("upsert local file: %v", err)
	}
	if err := store.RefreshItemSourceTypes(lib.ID); err != nil {
		t.Fatalf("refresh mixed: %v", err)
	}
	if got, _ := store.ItemByID(mixedShow.ID); got.SourceType != "" {
		t.Errorf("mixed show source_type = %q, want empty", got.SourceType)
	}
	if got, _ := store.ItemByID(mixedSeason.ID); got.SourceType != "" {
		t.Errorf("mixed season source_type = %q, want empty", got.SourceType)
	}
	if got, _ := store.ItemByID(mixedEp1.ID); got.SourceType != "bilibili" {
		t.Errorf("mixed episode 1 source_type = %q, want bilibili", got.SourceType)
	}
	if got, _ := store.ItemByID(mixedEp2.ID); got.SourceType != "local" {
		t.Errorf("mixed episode 2 source_type = %q, want local", got.SourceType)
	}
}

// A database from before the badge existed has no source_type column. Opening
// it must add the column and fill it, so a library does not lose its badges (or
// need a rescan) just because the server was upgraded.
func TestOpenBackfillsItemSourceTypes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-source-type.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	stmts := []string{
		`CREATE TABLE sources (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, type TEXT NOT NULL,
			config TEXT NOT NULL DEFAULT '{}', created_at TIMESTAMP NOT NULL)`,
		`CREATE TABLE libraries (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, type TEXT NOT NULL,
			source_id INTEGER NOT NULL, path TEXT NOT NULL DEFAULT '/', providers TEXT NOT NULL DEFAULT '[]',
			created_at TIMESTAMP NOT NULL)`,
		`CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, library_id INTEGER NOT NULL, type TEXT NOT NULL,
			parent_id INTEGER, name TEXT NOT NULL, original_title TEXT DEFAULT '', year INTEGER DEFAULT 0,
			tmdb_id INTEGER DEFAULT 0, overview TEXT DEFAULT '', poster_path TEXT DEFAULT '',
			backdrop_path TEXT DEFAULT '', still_path TEXT DEFAULT '', rating REAL DEFAULT 0,
			genres TEXT DEFAULT '[]', season INTEGER DEFAULT 0, episode INTEGER DEFAULT 0,
			air_date TEXT DEFAULT '', added_at TIMESTAMP NOT NULL)`,
		`CREATE TABLE files (id INTEGER PRIMARY KEY AUTOINCREMENT, item_id INTEGER NOT NULL, source_id INTEGER NOT NULL,
			path TEXT NOT NULL, size INTEGER DEFAULT 0, mtime INTEGER DEFAULT 0, probe TEXT DEFAULT '',
			added_at TIMESTAMP NOT NULL, UNIQUE(source_id, path))`,
	}
	for _, s := range stmts {
		if _, err := old.Exec(s); err != nil {
			t.Fatalf("legacy schema: %v", err)
		}
	}
	now := time.Now()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := old.Exec(q, args...); err != nil {
			t.Fatalf("legacy insert: %v", err)
		}
	}
	must(`INSERT INTO sources (id,name,type,config,created_at) VALUES (1,'B站','bilibili','{}',?)`, now)
	must(`INSERT INTO libraries (id,name,type,source_id,path,providers,created_at) VALUES (1,'番剧','series',1,'/','[]',?)`, now)
	must(`INSERT INTO items (id,library_id,type,parent_id,name,added_at) VALUES (1,1,'show',NULL,'鬼滅之刃',?)`, now)
	must(`INSERT INTO items (id,library_id,type,parent_id,name,season,added_at) VALUES (2,1,'season',1,'第一季',1,?)`, now)
	must(`INSERT INTO items (id,library_id,type,parent_id,name,season,episode,added_at) VALUES (3,1,'episode',2,'第 1 集',1,1,?)`, now)
	must(`INSERT INTO files (id,item_id,source_id,path,size,mtime,added_at) VALUES (1,3,1,'/e1.mp4',1,1,?)`, now)
	if err := old.Close(); err != nil {
		t.Fatalf("close legacy: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy database: %v", err)
	}
	defer store.DB.Close()
	for _, id := range []int64{1, 2, 3} {
		it, err := store.ItemByID(id)
		if err != nil {
			t.Fatalf("item %d after migration: %v", id, err)
		}
		if it.SourceType != "bilibili" {
			t.Errorf("item %d (%s) source_type = %q, want bilibili after backfill", id, it.Type, it.SourceType)
		}
	}
}
