package files_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
	"github.com/odrazdotxyz/odraz-go-sdk/internal/memfs"
)

var ctx = context.Background()

func put(t *testing.T, fs files.FS, p, c string) {
	t.Helper()
	if err := files.Upload(ctx, fs, p, strings.NewReader(c), files.WriteOptions{Size: int64(len(c))}); err != nil {
		t.Fatal(err)
	}
}

func TestUploadCreatesParents(t *testing.T) {
	fs := memfs.New()
	put(t, fs, "/a/b/c.txt", "x")
	if st, err := fs.Stat(ctx, "/a/b"); err != nil || !st.IsDir {
		t.Fatalf("parent not created: %v", err)
	}
	err := files.Upload(ctx, fs, "/a/b/c.txt", strings.NewReader("y"), files.WriteOptions{Size: 1})
	if !errors.Is(err, odraz.ErrExists) {
		t.Errorf("overwrite default must be false: %v", err)
	}
	if err := files.Upload(ctx, fs, "/../x", strings.NewReader("y"), files.WriteOptions{}); !errors.Is(err, odraz.ErrInvalidPath) {
		t.Errorf("escape: %v", err)
	}
	if err := files.Upload(ctx, fs, "/top.txt", strings.NewReader("y"), files.WriteOptions{Size: 1}); err != nil {
		t.Errorf("root-level upload: %v", err)
	}
}

func TestUploadFileAndDownloadFile(t *testing.T) {
	fs := memfs.New()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	data := bytes.Repeat([]byte("0123456789"), 100000)
	os.WriteFile(src, data, 0o644)
	if err := files.UploadFile(ctx, fs, "/up/file.bin", src, files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.bin")
	var last int64
	if err := files.DownloadFile(ctx, fs, "/up/file.bin", dst, files.DownloadOptions{Progress: func(d, _ int64) { last = d }}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, data) || last != int64(len(data)) {
		t.Errorf("content/progress wrong: last=%d", last)
	}
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Error(".part must be gone")
	}
}

func TestDownloadResume(t *testing.T) {
	fs := memfs.New()
	put(t, fs, "/f.bin", "0123456789")
	time.Sleep(10 * time.Millisecond)
	dst := filepath.Join(t.TempDir(), "f.bin")
	// The .part content differs from the remote on purpose: a real resume keeps
	// it and appends only bytes 5..9, a restart would overwrite it.
	os.WriteFile(dst+".part", []byte("ABCDE"), 0o644) // newer than the remote file
	if err := files.DownloadFile(ctx, fs, "/f.bin", dst, files.DownloadOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "ABCDE56789" {
		t.Errorf("resume: content = %q, want ABCDE56789", got)
	}
	// A stale .part (older than the remote file) must restart, not corrupt.
	os.WriteFile(dst+".part", []byte("XXXXX"), 0o644)
	os.Chtimes(dst+".part", time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	if err := files.DownloadFile(ctx, fs, "/f.bin", dst, files.DownloadOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "0123456789" {
		t.Errorf("stale part not discarded: %q", got)
	}
	// Download of a folder is rejected.
	fs.Mkdir(ctx, "/d")
	if err := files.DownloadFile(ctx, fs, "/d", dst, files.DownloadOptions{}); !errors.Is(err, odraz.ErrInvalidPath) {
		t.Errorf("folder: %v", err)
	}
}

func TestDownloadOffset(t *testing.T) {
	fs := memfs.New()
	put(t, fs, "/f", "abcdef")
	var b bytes.Buffer
	if err := files.Download(ctx, fs, "/f", &b, files.DownloadOptions{Offset: 3}); err != nil || b.String() != "def" {
		t.Errorf("got %q, %v", b.String(), err)
	}
}

func TestRename(t *testing.T) {
	fs := memfs.New()
	put(t, fs, "/d/a.txt", "x")
	put(t, fs, "/d/b.txt", "y")
	if err := files.Rename(ctx, fs, "/d/a.txt", "c.txt", false); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat(ctx, "/d/c.txt"); err != nil {
		t.Error("renamed file missing")
	}
	if err := files.Rename(ctx, fs, "/d/c.txt", "b.txt", false); !errors.Is(err, odraz.ErrExists) {
		t.Errorf("rename onto existing: %v", err)
	}
	for _, bad := range []string{"", "a/b", "..", "."} {
		if err := files.Rename(ctx, fs, "/d/c.txt", bad, true); !errors.Is(err, odraz.ErrInvalidPath) {
			t.Errorf("name %q: %v", bad, err)
		}
	}
}

func TestWalk(t *testing.T) {
	fs := memfs.New()
	for _, p := range []string{"/a/1.txt", "/a/b/2.txt", "/a/b/c/3.txt", "/z.txt", "/skip/hidden.txt", "/skip/x/y.txt"} {
		put(t, fs, p, "x")
	}
	var seen []string
	err := files.Walk(ctx, fs, "/", func(e files.Entry) error {
		seen = append(seen, e.Path)
		if e.Path == "/skip" {
			return files.SkipDir
		}
		return nil
	})
	want := "/ /a /a/1.txt /a/b /a/b/2.txt /a/b/c /a/b/c/3.txt /skip /z.txt"
	if err != nil || strings.Join(seen, " ") != want {
		t.Errorf("walk = %v, %v", seen, err)
	}
	stop := errors.New("stop")
	n := 0
	if err := files.Walk(ctx, fs, "/a", func(files.Entry) error { n++; return stop }); !errors.Is(err, stop) || n != 1 {
		t.Errorf("stop: n=%d err=%v", n, err)
	}
	// SkipDir on a file skips the rest of its folder.
	seen = nil
	files.Walk(ctx, fs, "/a", func(e files.Entry) error {
		seen = append(seen, e.Path)
		if e.Path == "/a/1.txt" {
			return files.SkipDir
		}
		return nil
	})
	if strings.Join(seen, " ") != "/a /a/1.txt" {
		t.Errorf("file SkipDir = %v", seen)
	}
	if err := files.Walk(ctx, fs, "/nope", func(files.Entry) error { return nil }); !errors.Is(err, odraz.ErrNotFound) {
		t.Errorf("missing root: %v", err)
	}
}

func TestWrapKeepsCopier(t *testing.T) {
	if _, ok := files.Wrap(memfs.New()).(files.Copier); ok {
		t.Error("memfs has no Copier; Wrap must not invent one")
	}
}

func TestReadOnly(t *testing.T) {
	inner := memfs.New()
	put(t, inner, "/a.txt", "x")
	fs := files.ReadOnly(inner)
	if !fs.Capabilities().ReadOnly {
		t.Error("capabilities must report ReadOnly")
	}
	if st, err := fs.Stat(ctx, "/a.txt"); err != nil || st.Size != 1 {
		t.Errorf("reads must work: %v", err)
	}
	for name, err := range map[string]error{
		"write":  fs.Write(ctx, "/b", strings.NewReader("y"), files.WriteOptions{}),
		"mkdir":  fs.Mkdir(ctx, "/d"),
		"move":   fs.Move(ctx, "/a.txt", "/c", true),
		"delete": fs.Delete(ctx, "/a.txt", true),
		"copy":   files.Copy(ctx, fs, "/a.txt", "/e", false),
		"upload": files.Upload(ctx, fs, "/f/g", strings.NewReader("y"), files.WriteOptions{}),
	} {
		if !errors.Is(err, odraz.ErrPermission) {
			t.Errorf("%s: err = %v, want ErrPermission", name, err)
		}
	}
	if _, err := inner.Stat(ctx, "/a.txt"); err != nil {
		t.Error("source must be untouched")
	}
}

// previewFS is a minimal FS with the optional Previewer.
type previewFS struct {
	files.FS
	gotW, gotH int
}

func (p *previewFS) Preview(_ context.Context, path string, w, h int) (io.ReadCloser, string, error) {
	p.gotW, p.gotH = w, h
	return io.NopCloser(strings.NewReader("png:" + path)), "image/png", nil
}

func TestPreviewHelper(t *testing.T) {
	for _, c := range []struct{ w, h int }{{0, 10}, {10, 0}, {-1, 5}, {files.MaxPreviewSize + 1, 10}} {
		if _, _, err := files.Preview(ctx, &previewFS{}, "/a.png", c.w, c.h); !errors.Is(err, odraz.ErrInvalidPath) {
			t.Errorf("Preview(%dx%d) = %v, want ErrInvalidPath", c.w, c.h, err)
		}
	}
	if _, _, err := files.Preview(ctx, memfs.New(), "/a.png", 64, 64); !errors.Is(err, odraz.ErrUnsupported) {
		t.Errorf("FS without Previewer: %v, want ErrUnsupported", err)
	}
	pf := &previewFS{}
	rc, ct, err := files.Preview(ctx, pf, "/a.png", 64, 48)
	if err != nil || ct != "image/png" || pf.gotW != 64 || pf.gotH != 48 {
		t.Fatalf("Preview = %v %q %dx%d", err, ct, pf.gotW, pf.gotH)
	}
	rc.Close()
}

// Wrap and ReadOnly must not hide a Previewer; Wrap cleans the path first.
func TestWrapperKeepsPreviewer(t *testing.T) {
	inner := &previewFS{}
	for name, fs := range map[string]files.FS{"wrap": files.Wrap(inner), "readonly": files.ReadOnly(inner), "readonly+wrap": files.ReadOnly(files.Wrap(inner))} {
		if _, ok := fs.(files.Previewer); !ok {
			t.Errorf("%s: lost the Previewer", name)
			continue
		}
		rc, _, err := files.Preview(ctx, fs, "/a/../b.png", 10, 10)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		b, _ := io.ReadAll(rc)
		if name != "readonly" && string(b) != "png:/b.png" {
			t.Errorf("%s: path = %q, want the cleaned %q", name, b, "png:/b.png")
		}
	}
	if _, ok := files.Wrap(memfs.New()).(files.Previewer); ok {
		t.Error("an FS without a preview must not gain one")
	}
	// The same FS with both optional interfaces keeps both.
	if _, ok := files.Wrap(&copyPreviewFS{}).(files.Copier); !ok {
		t.Error("Wrap lost the Copier when the FS also has a Previewer")
	}
	if _, ok := files.Wrap(&copyPreviewFS{}).(files.Previewer); !ok {
		t.Error("Wrap lost the Previewer when the FS also has a Copier")
	}
}

type copyPreviewFS struct{ previewFS }

func (*copyPreviewFS) Copy(context.Context, string, string, bool) error { return nil }
