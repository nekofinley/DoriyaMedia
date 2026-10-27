package source

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLocalDriverRejectsPathsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := NewLocal(root)

	paths := []string{
		"../secret.txt",
		"nested/../../secret.txt",
		`\\server\share\secret.txt`,
		"//server/share/secret.txt",
		`C:\Windows\win.ini`,
		"C:/Windows/win.ini",
		"nested/clip.mp4\x00.png",
	}
	if runtime.GOOS == "windows" {
		paths = append(paths, secret)
	}
	for _, path := range paths {
		if _, err := driver.resolve(path); !errors.Is(err, errLocalPathOutsideRoot) {
			t.Errorf("resolve(%q) error = %v, want escape rejection", path, err)
		}
	}
}

func TestLocalDriverAllowsContainedPaths(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nested", "clip.mp4")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := NewLocal(root)

	got, err := driver.resolve("/nested/clip.mp4")
	if err != nil {
		t.Fatalf("resolve contained path: %v", err)
	}
	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resolved path = %q, want %q", got, want)
	}
	file, err := driver.Open(context.Background(), "/nested/clip.mp4")
	if err != nil {
		t.Fatalf("open contained path: %v", err)
	}
	_ = file.Close()
}

func TestLocalDriverRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	driver := NewLocal(root)
	if _, err := driver.resolve("escape.txt"); !errors.Is(err, errLocalPathOutsideRoot) {
		t.Fatalf("symlink escape error = %v, want escape rejection", err)
	}
	file, err := driver.Open(context.Background(), "escape.txt")
	if err == nil {
		_ = file.Close()
		t.Fatal("Open followed a symlink outside the root")
	}
}

func TestLocalDriverRejectsWindowsAliasPaths(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path aliases")
	}
	driver := NewLocal(t.TempDir())
	for _, path := range []string{
		"movie.mkv:secret",
		"movie.mkv::$DATA",
		"NUL",
		"nul.txt",
		"NUL .txt",
		"CON .txt",
		"sub/CON",
		"sub/COM1.mkv",
		"trailing-space ",
		"trailing-dot.",
		`nested\.. `,
	} {
		if _, err := driver.resolve(path); !errors.Is(err, errLocalPathOutsideRoot) {
			t.Errorf("resolve(%q) error = %v, want Windows alias rejection", path, err)
		}
		if _, err := driver.openFile(path); !errors.Is(err, errLocalPathOutsideRoot) {
			t.Errorf("openFile(%q) error = %v, want Windows alias rejection", path, err)
		}
	}
}

func TestLocalWindowsPathSafetyKeepsOrdinaryNames(t *testing.T) {
	for _, path := range []string{
		"movie.mkv",
		"nested/movie.mp4",
		"console documentary.mkv",
		"comet.mkv",
		"lpt.txt",
	} {
		if !localWindowsPathSafe(path) {
			t.Errorf("localWindowsPathSafe(%q) = false", path)
		}
	}
}

func TestLocalDriverRejectsDirectoryLinkEscapes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		junction bool
	}{
		{name: "symlink"},
		{name: "junction", junction: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			const secret = "outside-secret"
			if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(secret), 0o600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, "escape")
			makeDirectoryLink(t, link, outside, tc.junction)

			driver := NewLocal(root)
			file, err := driver.Open(context.Background(), "/escape/secret.txt")
			if err == nil {
				_ = file.Close()
				t.Fatal("Open escaped through a directory link")
			}
			if _, err := driver.List(context.Background(), "/escape"); err == nil {
				t.Fatal("List escaped through a directory link")
			}

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/escape/secret.txt", nil)
			driver.Serve(rec, req, "/escape/secret.txt")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("Serve status = %d, want %d", rec.Code, http.StatusNotFound)
			}
			if bytes.Contains(rec.Body.Bytes(), []byte(secret)) {
				t.Fatal("Serve returned bytes from outside the root")
			}
		})
	}
}

func TestLocalDriverList(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "media")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"b.mkv", "a.mp4"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := NewLocal(root).List(context.Background(), "/media")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 || entries[0].Name != "a.mp4" || entries[1].Name != "b.mkv" {
		t.Fatalf("List entries = %#v, want sorted a.mp4 and b.mkv", entries)
	}
}

func TestLocalDriverKeepsOpenedObjectsAcrossRename(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "clip.bin")
		if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		driver := NewLocal(root)
		file, err := driver.Open(context.Background(), "/clip.bin")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()

		if err := os.Rename(path, filepath.Join(root, "clip.saved")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "original" {
			t.Fatalf("opened file read %q after path replacement, want original", got)
		}
	})

	t.Run("directory", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "media")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "original.txt"), []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		driver := NewLocal(root)
		dirFile, err := driver.openFile("/media")
		if err != nil {
			t.Fatal(err)
		}
		defer dirFile.Close()

		if err := os.Rename(dir, filepath.Join(root, "media.saved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "replacement.txt"), []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
		entries, err := dirFile.ReadDir(-1)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "original.txt" {
			t.Fatalf("opened directory entries = %#v, want original.txt", entries)
		}
	})
}

func TestLocalDriverServeRangeUsesOpenedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "clip.bin")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := NewLocal(root)

	tests := []struct {
		name        string
		method      string
		rangeHeader string
		wantStatus  int
		wantBody    string
		wantRange   string
	}{
		{
			name:        "closed",
			method:      http.MethodGet,
			rangeHeader: "bytes=2-5",
			wantStatus:  http.StatusPartialContent,
			wantBody:    "2345",
			wantRange:   "bytes 2-5/10",
		},
		{
			name:        "suffix",
			method:      http.MethodGet,
			rangeHeader: "bytes=-3",
			wantStatus:  http.StatusPartialContent,
			wantBody:    "789",
			wantRange:   "bytes 7-9/10",
		},
		{
			name:        "unsatisfiable",
			method:      http.MethodGet,
			rangeHeader: "bytes=20-30",
			wantStatus:  http.StatusRequestedRangeNotSatisfiable,
			wantRange:   "bytes */10",
		},
		{
			name:       "head",
			method:     http.MethodHead,
			wantStatus: http.StatusOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, "/clip.bin", nil)
			if tc.rangeHeader != "" {
				req.Header.Set("Range", tc.rangeHeader)
			}
			driver.Serve(rec, req, "/clip.bin")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantRange != "" && rec.Header().Get("Content-Range") != tc.wantRange {
				t.Fatalf("Content-Range = %q, want %q", rec.Header().Get("Content-Range"), tc.wantRange)
			}
			if tc.method == http.MethodGet &&
				tc.wantStatus == http.StatusPartialContent &&
				rec.Body.String() != tc.wantBody {
				t.Fatalf("body = %q, want %q", rec.Body.String(), tc.wantBody)
			}
			if tc.method == http.MethodHead && rec.Body.Len() != 0 {
				t.Fatalf("HEAD body length = %d, want 0", rec.Body.Len())
			}
		})
	}

	file, err := driver.openFile("/clip.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Rename(path, filepath.Join(root, "clip.saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/clip.bin", nil)
	req.Header.Set("Range", "bytes=0-5")
	serveLocalFile(rec, req, "clip.bin", file)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "012345" {
		t.Fatalf("served path replacement: status = %d, body = %q", rec.Code, rec.Body.String())
	}
}

func makeDirectoryLink(t *testing.T, link, target string, junction bool) {
	t.Helper()
	if junction {
		if runtime.GOOS != "windows" {
			t.Skip("junctions are Windows-only")
		}
		out, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, target).CombinedOutput()
		if err != nil {
			t.Skipf("junction unavailable: %v (%s)", err, out)
		}
		t.Cleanup(func() { _ = os.Remove(link) })
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
}
