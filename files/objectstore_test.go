package files_test

import (
	"context"
	"errors"
	"io"
	"testing"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
	"github.com/odrazdotxyz/odraz-go-sdk/internal/memfs"
)

type stubStore struct {
	files.FS
	purged []string
}

func (s *stubStore) NewCheck(int64) files.Check { return nil }
func (s *stubStore) Purge(_ context.Context, p string) error {
	s.purged = append(s.purged, p)
	return nil
}

type stubStoreCopier struct{ *stubStore }

func (stubStoreCopier) Copy(context.Context, string, string, bool) error { return nil }
func (stubStoreCopier) Preview(context.Context, string, int, int) (io.ReadCloser, string, error) {
	return nil, "", nil
}
func (stubStoreCopier) SHA256(context.Context, string) (string, error) { return "", nil }

func TestWrapPreservesObjectStore(t *testing.T) {
	in := &stubStore{FS: memfs.New()}
	for name, inner := range map[string]files.FS{"alone": in, "with every other interface": stubStoreCopier{in}} {
		os, ok := files.Wrap(inner).(files.ObjectStore)
		if !ok {
			t.Fatalf("%s: ObjectStore lost", name)
		}
		in.purged = nil
		if err := os.Purge(context.Background(), "a//b/"); err != nil || len(in.purged) != 1 || in.purged[0] != "/a/b" {
			t.Fatalf("%s: purge got %q, %v", name, in.purged, err)
		}
		if err := os.Purge(context.Background(), "../x"); !errors.Is(err, odraz.ErrInvalidPath) {
			t.Fatalf("%s: escaping path: %v", name, err)
		}
	}
	all := files.Wrap(stubStoreCopier{in})
	_, c := all.(files.Copier)
	_, p := all.(files.Previewer)
	_, h := all.(files.Hasher)
	if !c || !p || !h {
		t.Fatalf("other interfaces lost: copier=%v previewer=%v hasher=%v", c, p, h)
	}
	if _, ok := files.Wrap(memfs.New()).(files.ObjectStore); ok {
		t.Fatal("Wrap invented ObjectStore")
	}
}

func TestReadOnlyPurgeIsPermission(t *testing.T) {
	in := &stubStore{FS: memfs.New()}
	os, ok := files.ReadOnly(in).(files.ObjectStore)
	if !ok {
		t.Fatal("ReadOnly dropped ObjectStore (checking is a read)")
	}
	if err := os.Purge(context.Background(), "/a"); !errors.Is(err, odraz.ErrPermission) || len(in.purged) != 0 {
		t.Fatalf("purge on read-only: %v, calls %v", err, in.purged)
	}
}
