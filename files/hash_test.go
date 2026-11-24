package files_test

import (
	"context"
	"io"
	"testing"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
	"github.com/odrazdotxyz/odraz-go-sdk/internal/memfs"
)

const helloSHA = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"

// hashFS is an FS stub whose optional interfaces are controlled by embedding.
type hashFS struct {
	files.FS
	sum     string
	err     error
	reads   int
	gotPath string
}

func (h *hashFS) SHA256(_ context.Context, p string) (string, error) {
	h.gotPath = p
	return h.sum, h.err
}

func (h *hashFS) Read(c context.Context, p string, off, n int64) (io.ReadCloser, error) {
	h.reads++
	return h.FS.Read(c, p, off, n)
}

type hashCopyFS struct{ *hashFS }

func (hashCopyFS) Copy(context.Context, string, string, bool) error { return nil }

func TestSHA256FallsBackToReading(t *testing.T) {
	fs := memfs.New()
	put(t, fs, "/a.txt", "hello")
	got, err := files.SHA256(ctx, fs, "/a.txt")
	if err != nil || got != helloSHA {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestSHA256UsesHasher(t *testing.T) {
	m := memfs.New()
	put(t, m, "/a.txt", "hello")
	h := &hashFS{FS: m, sum: "abc"}
	got, err := files.SHA256(ctx, h, "/a.txt")
	if err != nil || got != "abc" || h.reads != 0 {
		t.Fatalf("got %q, %v, reads=%d", got, err, h.reads)
	}
}

func TestSHA256FallsBackWhenUnsupported(t *testing.T) {
	m := memfs.New()
	put(t, m, "/a.txt", "hello")
	h := &hashFS{FS: m, err: odraz.ErrUnsupported}
	got, err := files.SHA256(ctx, h, "/a.txt")
	if err != nil || got != helloSHA || h.reads != 1 {
		t.Fatalf("got %q, %v, reads=%d", got, err, h.reads)
	}
}

func TestWrapPreservesHasher(t *testing.T) {
	h := &hashFS{FS: memfs.New(), sum: "abc"}
	w := files.Wrap(h)
	hs, ok := w.(files.Hasher)
	if !ok {
		t.Fatal("Wrap dropped Hasher")
	}
	if _, ok := w.(files.Copier); ok {
		t.Error("Wrap invented Copier")
	}
	if _, ok := w.(files.Previewer); ok {
		t.Error("Wrap invented Previewer")
	}
	if s, err := hs.SHA256(ctx, "/x/../y.txt"); err != nil || s != "abc" || h.gotPath != "/y.txt" {
		t.Fatalf("got %q %v path %q", s, err, h.gotPath)
	}
	if _, err := hs.SHA256(ctx, "/../x"); err == nil {
		t.Error("escaping path accepted")
	}
	wc := files.Wrap(hashCopyFS{h})
	if _, ok := wc.(files.Copier); !ok {
		t.Error("Wrap dropped Copier")
	}
	if _, ok := wc.(files.Hasher); !ok {
		t.Error("Wrap dropped Hasher with Copier")
	}
	if _, ok := files.Wrap(memfs.New()).(files.Hasher); ok {
		t.Error("Wrap invented Hasher")
	}
}
