package scan

import (
	"database/sql"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

var ErrScanQueueFull = errors.New("扫描队列已满,请稍后再试")

type scanScope struct {
	SourceID int64
	Path     string
	Root     string
}

type scanJob struct {
	lib   *models.Library
	scope *scanScope
}

// ScanLibraryPath enqueues a recursive directory scan within a configured
// library source. sourceID may be omitted only when the path resolves uniquely.
// Paths use the source's virtual namespace, not the server's filesystem root.
func (sc *Scanner) ScanLibraryPath(lib *models.Library, sourceID int64, dir string) error {
	scope, err := sc.resolveScanScope(lib, sourceID, dir)
	if err != nil {
		return err
	}
	return sc.enqueue(scanJob{lib: lib, scope: scope})
}

func cleanScanPath(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" || !strings.HasPrefix(dir, "/") || strings.ContainsAny(dir, "\\\x00") {
		return "", fmt.Errorf("扫描路径必须是以 / 开头的源内目录路径")
	}
	for _, part := range strings.Split(dir, "/") {
		if part == ".." {
			return "", fmt.Errorf("扫描路径不能包含 ..")
		}
	}
	return path.Clean(dir), nil
}

func pathWithin(p, root string) bool {
	// A STRM URL is not a virtual source path, even when scanning "/".
	if !strings.HasPrefix(p, "/") {
		return false
	}
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

func (sc *Scanner) resolveScanScope(lib *models.Library, sourceID int64, dir string) (*scanScope, error) {
	dir, err := cleanScanPath(dir)
	if err != nil {
		return nil, err
	}
	if sourceID < 0 {
		return nil, fmt.Errorf("存储源无效")
	}
	places, err := sc.Store.LibrarySources(lib.ID)
	if err != nil {
		return nil, fmt.Errorf("读取媒体库存储源失败: %w", err)
	}
	if len(places) == 0 {
		places = []models.LibrarySource{{LibraryID: lib.ID, SourceID: lib.SourceID, Path: lib.Path}}
	}
	var scope *scanScope
	for _, place := range places {
		if sourceID != 0 && sourceID != place.SourceID {
			continue
		}
		root := place.Path
		if root == "" {
			root = "/"
		}
		root, err = cleanScanPath(root)
		if err != nil || !pathWithin(dir, root) {
			continue
		}
		if scope != nil {
			if scope.SourceID != place.SourceID {
				return nil, fmt.Errorf("路径匹配多个存储源,请指定 source_id")
			}
			// Overlapping configured roots on the same source use the most
			// specific root, preserving the normal scan's naming context.
			if len(root) <= len(scope.Root) {
				continue
			}
		}
		scope = &scanScope{SourceID: place.SourceID, Path: dir, Root: root}
	}
	if scope == nil {
		return nil, fmt.Errorf("扫描路径不在媒体库已配置的存储源目录内")
	}
	return scope, nil
}

// validateDriver checks the real directory as well as the virtual path. A
// directly selected local symlink must not let a subtree scan escape a library
// root; the ordinary recursive walk does not follow directory symlinks.
func (scope *scanScope) validateDriver(drv source.Driver) error {
	local, ok := drv.(*source.LocalDriver)
	if !ok {
		return nil
	}
	resolve := func(dir string) (string, error) {
		return filepath.EvalSymlinks(filepath.Join(local.Root, filepath.FromSlash(strings.TrimPrefix(dir, "/"))))
	}
	root, err := resolve(scope.Root)
	if err != nil {
		return fmt.Errorf("读取媒体库根目录失败: %w", err)
	}
	target, err := resolve(scope.Path)
	if err != nil {
		return fmt.Errorf("读取扫描目录失败: %w", err)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("扫描目录的真实路径不在媒体库配置目录内")
	}
	return nil
}

// keepScopedFiles protects files already owned by another library. The store's
// source/path key is global, so upserting them would otherwise steal their item.
func (sc *Scanner) keepScopedFiles(lib *models.Library, records []fileRec, prog *models.ScanProgress) ([]fileRec, error) {
	kept := make([]fileRec, 0, len(records))
	for _, rec := range records {
		f, err := sc.Store.FileBySourcePath(rec.SourceID, rec.Path)
		if errors.Is(err, sql.ErrNoRows) {
			kept = append(kept, rec)
			continue
		}
		if err != nil {
			return nil, err
		}
		it, err := sc.Store.ItemByID(f.ItemID)
		if err != nil {
			return nil, err
		}
		if it.LibraryID != lib.ID {
			prog.AddEvent("warn", "跳过已属于其他媒体库的文件 "+rec.Path)
			continue
		}
		if rec.Strm {
			// The stored URL does not record which .strm directory imported it.
			// Do not rebind a pre-existing alias to a new group in a path scan.
			prog.AddEvent("warn", "保留已入库 STRM 链接的原有归属 "+rec.Path)
			continue
		}
		kept = append(kept, rec)
	}
	return kept, nil
}

// scannedFiles resolves only records produced by this walk. Files excluded by
// organization, or belonging to another library, cannot reach scoped hooks.
func (sc *Scanner) scannedFiles(lib *models.Library, records []fileRec) []models.MediaFile {
	var files []models.MediaFile
	seen := map[int64]bool{}
	records = dropExcluded(records,
		excludeMatcher(EffectiveExcludes(lib, sc.Cfg)),
		excludeMatcher(EffectiveExcludeFolders(lib, sc.Cfg)))
	for _, rec := range records {
		f, err := sc.Store.FileBySourcePath(rec.SourceID, rec.Path)
		if err != nil || f == nil || seen[f.ID] {
			continue
		}
		it, err := sc.Store.ItemByID(f.ItemID)
		if err != nil || it.LibraryID != lib.ID {
			continue
		}
		seen[f.ID] = true
		files = append(files, *f)
	}
	return files
}

// pruneScanItems follows only parents of removed files. A path scan must not
// opportunistically delete unrelated orphan items elsewhere in the library.
func (sc *Scanner) pruneScanItems(libID int64, itemIDs []int64) error {
	seen := map[int64]bool{}
	for len(itemIDs) > 0 {
		id := itemIDs[0]
		itemIDs = itemIDs[1:]
		if seen[id] {
			continue
		}
		it, err := sc.Store.ItemByID(id)
		if err != nil {
			return err
		}
		if it.LibraryID != libID {
			continue
		}
		result, err := sc.Store.DB.Exec(`DELETE FROM items WHERE id=? AND library_id=?
			AND NOT EXISTS (SELECT 1 FROM files WHERE item_id=?)
			AND NOT EXISTS (SELECT 1 FROM items WHERE parent_id=?)`, id, libID, id, id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		// An ancestor that still has children can be reconsidered after its
		// last sibling is removed; only remember successful deletions.
		if n > 0 {
			seen[id] = true
			if it.ParentID != nil {
				itemIDs = append(itemIDs, *it.ParentID)
			}
		}
	}
	return nil
}
