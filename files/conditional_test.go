package files_test

import (
	"context"
	"errors"
	"testing"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
	"github.com/odrazdotxyz/odraz-go-sdk/internal/memfs"
)

type conditional struct {
	files.FS
	calls []string
}

func (c *conditional) DeleteIfMatch(_ context.Context, p, etag string) error {
	c.calls = append(c.calls, p+" "+etag)
	return nil
}

func TestWrapDeleteIfMatch(t *testing.T) {
	ctx := context.Background()
	in := &conditional{FS: memfs.New()}
	w, ok := files.Wrap(in).(files.ConditionalDeleter)
	if !ok {
		t.Fatal("ConditionalDeleter lost")
	}
	if err := w.DeleteIfMatch(ctx, "a//b/", `"e"`); err != nil || len(in.calls) != 1 || in.calls[0] != `/a/b "e"` {
		t.Fatalf("calls %q, %v", in.calls, err)
	}
	if err := w.DeleteIfMatch(ctx, "../x", `"e"`); !errors.Is(err, odraz.ErrInvalidPath) {
		t.Errorf("escaping: %v", err)
	}
	for _, bad := range [][2]string{{"/a", ""}, {"/", `"e"`}} {
		if err := w.DeleteIfMatch(ctx, bad[0], bad[1]); !errors.Is(err, odraz.ErrInvalidPath) {
			t.Errorf("%q %q: %v", bad[0], bad[1], err)
		}
	}
	if len(in.calls) != 1 {
		t.Errorf("a refused call reached the backend: %q", in.calls)
	}
	// A wrapped FS always has the method; a backend without it says so and keeps the file.
	plain := memfs.New()
	if err := plain.Mkdir(ctx, "/keep"); err != nil {
		t.Fatal(err)
	}
	if err := files.Wrap(plain).(files.ConditionalDeleter).DeleteIfMatch(ctx, "/keep", `"e"`); !errors.Is(err, odraz.ErrUnsupported) {
		t.Errorf("unsupported: %v", err)
	}
	if _, err := plain.Stat(ctx, "/keep"); err != nil {
		t.Errorf("removed: %v", err)
	}
	ro := files.ReadOnly(in).(files.ConditionalDeleter)
	if err := ro.DeleteIfMatch(ctx, "/a", `"e"`); !errors.Is(err, odraz.ErrPermission) || len(in.calls) != 1 {
		t.Errorf("read-only: %v %q", err, in.calls)
	}
}
