package source

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

func jsonUnmarshal(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}

// LocalDriver serves a directory on the server's own filesystem.
type LocalDriver struct {
	Root string
}

func NewLocal(root string) *LocalDriver {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &LocalDriver{Root: filepath.Clean(root)}
}

var errLocalPathOutsideRoot = errors.New("local source path escapes root")

func (d *LocalDriver) relativePath(p string) (string, error) {
	if d == nil {
		return "", errLocalPathOutsideRoot
	}

	// Virtual paths may begin with one slash. Everything else must remain a
	// relative path: absolute paths, drive-qualified paths, UNC shares, and
	// parent traversal are never valid inside a local source.
	if strings.IndexByte(p, 0) >= 0 ||
		strings.HasPrefix(p, `\`) ||
		hasWindowsDrivePrefix(p) {
		return "", errLocalPathOutsideRoot
	}
	slashed := filepath.ToSlash(p)
	if strings.HasPrefix(slashed, "//") {
		return "", errLocalPathOutsideRoot
	}
	virtual := strings.TrimPrefix(slashed, "/")
	if hasWindowsDrivePrefix(virtual) {
		return "", errLocalPathOutsideRoot
	}
	relative := filepath.FromSlash(virtual)
	if filepath.IsAbs(relative) ||
		filepath.VolumeName(relative) != "" {
		return "", errLocalPathOutsideRoot
	}
	if runtime.GOOS == "windows" && !localWindowsPathSafe(relative) {
		return "", errLocalPathOutsideRoot
	}
	relative = filepath.Clean(relative)
	if relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errLocalPathOutsideRoot
	}
	return relative, nil
}

func hasWindowsDrivePrefix(path string) bool {
	if len(path) < 2 || path[1] != ':' {
		return false
	}
	return path[0] >= 'A' && path[0] <= 'Z' ||
		path[0] >= 'a' && path[0] <= 'z'
}

func (d *LocalDriver) resolve(p string) (string, error) {
	relative, err := d.relativePath(p)
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(d.Root)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	full := filepath.Clean(filepath.Join(root, relative))
	if !localPathWithin(root, full) {
		return "", errLocalPathOutsideRoot
	}

	// Lexical containment is not enough when a link inside the root points
	// somewhere else. Resolve the root and target before opening or serving it.
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	resolvedFull, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", err
	}
	if !localPathWithin(resolvedRoot, resolvedFull) {
		return "", errLocalPathOutsideRoot
	}
	return resolvedFull, nil
}

func (d *LocalDriver) openFile(path string) (*os.File, error) {
	relative, err := d.relativePath(path)
	if err != nil {
		return nil, err
	}
	return os.OpenInRoot(d.Root, relative)
}

func localPathWithin(root, full string) bool {
	rel, err := filepath.Rel(root, full)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// localWindowsPathSafe rejects Win32 aliases that would let a virtual path name
// something other than an ordinary file below the source root. ADS names,
// reserved device names and trailing-dot/space aliases are not needed by a
// media library and have historically confused containment checks.
func localWindowsPathSafe(relative string) bool {
	for _, component := range strings.FieldsFunc(relative, func(r rune) bool {
		return r == '\\' || r == '/'
	}) {
		if component == "" || component == "." {
			continue
		}
		if strings.ContainsRune(component, ':') ||
			strings.HasSuffix(component, ".") ||
			strings.HasSuffix(component, " ") {
			return false
		}
		name := component
		if dot := strings.IndexByte(name, '.'); dot >= 0 {
			name = name[:dot]
		}
		name = strings.TrimRight(name, " .")
		switch strings.ToUpper(name) {
		case "CON", "PRN", "AUX", "NUL", "CLOCK$", "CONIN$", "CONOUT$":
			return false
		}
		if len(name) == 4 &&
			(strings.EqualFold(name[:3], "COM") || strings.EqualFold(name[:3], "LPT")) &&
			name[3] >= '1' && name[3] <= '9' {
			return false
		}
	}
	return true
}

func (d *LocalDriver) List(ctx context.Context, dir string) ([]Entry, error) {
	dirFile, err := d.openFile(dir)
	if err != nil {
		return nil, err
	}
	defer dirFile.Close()
	entries, err := dirFile.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Entry{
			Name:  e.Name(),
			Path:  VirtualJoin(dir, e.Name()),
			IsDir: e.IsDir(),
			Size:  info.Size(),
			MTime: info.ModTime().Unix(),
		})
	}
	return out, nil
}

func (d *LocalDriver) DirectURL(ctx context.Context, path string) (string, map[string]string, error) {
	return "", nil, ErrNoDirect
}

func (d *LocalDriver) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	return d.openFile(path)
}

func (d *LocalDriver) ProbeURL(ctx context.Context, path string) (string, map[string]string, error) {
	full, err := d.resolve(path)
	if err != nil {
		return "", nil, err
	}
	return full, nil, nil
}

// Serve writes the file with full Range support (used for proxy playback).
func (d *LocalDriver) Serve(w http.ResponseWriter, r *http.Request, path string) {
	file, err := d.openFile(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	serveLocalFile(w, r, filepath.Base(filepath.FromSlash(path)), file)
}

func serveLocalFile(w http.ResponseWriter, r *http.Request, name string, file *os.File) {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, info.ModTime(), file)
}

func (d *LocalDriver) HasDirect() bool { return false }

// ErrNoDirect marks drivers that cannot produce a 302-able link.
var ErrNoDirect = errNoDirect{}

type errNoDirect struct{}

func (errNoDirect) Error() string { return "source does not support direct links" }
