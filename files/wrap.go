package files

import (
	"context"
	"fmt"
	"io"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
)

// Wrap returns an FS that normalizes every path with CleanPath before
// calling inner, so adapters never see unclean or root-escaping paths.
// Optional interfaces of inner (Copier, Previewer, Hasher, ObjectStore) are preserved.
//
// Go cannot add methods conditionally, so each combination needs its own
// type; they are built from independent parts, one per optional interface, so
// the combinations below hold no logic.
func Wrap(inner FS) FS {
	c := &cleanFS{inner}
	cp, hasCp := inner.(Copier)
	pv, hasPv := inner.(Previewer)
	hs, hasHs := inner.(Hasher)
	os, hasOs := inner.(ObjectStore)
	C, P, H, O := cleanCopy{cp}, cleanPreview{pv}, cleanHash{hs}, cleanStore{os}
	switch {
	case hasCp && hasPv && hasHs && hasOs:
		return &struct {
			*cleanFS
			cleanCopy
			cleanPreview
			cleanHash
			cleanStore
		}{c, C, P, H, O}
	case hasCp && hasPv && hasHs:
		return &struct {
			*cleanFS
			cleanCopy
			cleanPreview
			cleanHash
		}{c, C, P, H}
	case hasCp && hasPv && hasOs:
		return &struct {
			*cleanFS
			cleanCopy
			cleanPreview
			cleanStore
		}{c, C, P, O}
	case hasCp && hasHs && hasOs:
		return &struct {
			*cleanFS
			cleanCopy
			cleanHash
			cleanStore
		}{c, C, H, O}
	case hasPv && hasHs && hasOs:
		return &struct {
			*cleanFS
			cleanPreview
			cleanHash
			cleanStore
		}{c, P, H, O}
	case hasCp && hasPv:
		return &struct {
			*cleanFS
			cleanCopy
			cleanPreview
		}{c, C, P}
	case hasCp && hasHs:
		return &struct {
			*cleanFS
			cleanCopy
			cleanHash
		}{c, C, H}
	case hasCp && hasOs:
		return &struct {
			*cleanFS
			cleanCopy
			cleanStore
		}{c, C, O}
	case hasPv && hasHs:
		return &struct {
			*cleanFS
			cleanPreview
			cleanHash
		}{c, P, H}
	case hasPv && hasOs:
		return &struct {
			*cleanFS
			cleanPreview
			cleanStore
		}{c, P, O}
	case hasHs && hasOs:
		return &struct {
			*cleanFS
			cleanHash
			cleanStore
		}{c, H, O}
	case hasCp:
		return &struct {
			*cleanFS
			cleanCopy
		}{c, C}
	case hasPv:
		return &struct {
			*cleanFS
			cleanPreview
		}{c, P}
	case hasHs:
		return &struct {
			*cleanFS
			cleanHash
		}{c, H}
	case hasOs:
		return &struct {
			*cleanFS
			cleanStore
		}{c, O}
	}
	return c
}

type cleanStore struct{ os ObjectStore }

func (c cleanStore) NewCheck(size int64) Check { return cleanCheck{c.os.NewCheck(size)} }

func (c cleanStore) Purge(ctx context.Context, p string) error {
	p, err := CleanPath(p)
	if err != nil {
		return err
	}
	return c.os.Purge(ctx, p)
}

type cleanCheck struct{ Check }

func (c cleanCheck) Verify(ctx context.Context, p string) error {
	p, err := CleanPath(p)
	if err != nil {
		return err
	}
	return c.Check.Verify(ctx, p)
}

type cleanCopy struct{ cp Copier }

func (c cleanCopy) Copy(ctx context.Context, from, to string, ow bool) error {
	from, err := CleanPath(from)
	if err != nil {
		return err
	}
	to, err = CleanPath(to)
	if err != nil {
		return err
	}
	return c.cp.Copy(ctx, from, to, ow)
}

type cleanPreview struct{ pv Previewer }

func (c cleanPreview) Preview(ctx context.Context, p string, w, h int) (io.ReadCloser, string, error) {
	p, err := CleanPath(p)
	if err != nil {
		return nil, "", err
	}
	return c.pv.Preview(ctx, p, w, h)
}

type cleanHash struct{ hs Hasher }

func (c cleanHash) SHA256(ctx context.Context, p string) (string, error) {
	p, err := CleanPath(p)
	if err != nil {
		return "", err
	}
	return c.hs.SHA256(ctx, p)
}

type cleanFS struct{ in FS }

func (c *cleanFS) Stat(ctx context.Context, p string) (Entry, error) {
	p, err := CleanPath(p)
	if err != nil {
		return Entry{}, err
	}
	return c.in.Stat(ctx, p)
}

func (c *cleanFS) List(ctx context.Context, p string) ([]Entry, error) {
	p, err := CleanPath(p)
	if err != nil {
		return nil, err
	}
	return c.in.List(ctx, p)
}

func (c *cleanFS) Read(ctx context.Context, p string, off, n int64) (io.ReadCloser, error) {
	p, err := CleanPath(p)
	if err != nil {
		return nil, err
	}
	return c.in.Read(ctx, p, off, n)
}

func (c *cleanFS) Write(ctx context.Context, p string, r io.Reader, o WriteOptions) error {
	p, err := CleanPath(p)
	if err != nil {
		return err
	}
	return c.in.Write(ctx, p, r, o)
}

func (c *cleanFS) Mkdir(ctx context.Context, p string) error {
	p, err := CleanPath(p)
	if err != nil {
		return err
	}
	return c.in.Mkdir(ctx, p)
}

func (c *cleanFS) Move(ctx context.Context, from, to string, ow bool) error {
	from, err := CleanPath(from)
	if err != nil {
		return err
	}
	to, err = CleanPath(to)
	if err != nil {
		return err
	}
	return c.in.Move(ctx, from, to, ow)
}

func (c *cleanFS) Delete(ctx context.Context, p string, rec bool) error {
	p, err := CleanPath(p)
	if err != nil {
		return err
	}
	return c.in.Delete(ctx, p, rec)
}

func (c *cleanFS) Capabilities() Capabilities { return c.in.Capabilities() }

// ReadOnly returns an FS that serves reads from inner and rejects every
// mutation with an error matching odraz.ErrPermission. Capabilities report
// ReadOnly. Optional write interfaces (Copier) are not exposed; read-only
// ones (Previewer, and the checking half of ObjectStore) are kept.
func ReadOnly(inner FS) FS {
	ro := &readOnlyFS{inner}
	pv, hasPv := inner.(Previewer)
	os, hasOs := inner.(ObjectStore)
	switch {
	case hasPv && hasOs:
		return &struct {
			*readOnlyPreviewerFS
			readOnlyStore
		}{&readOnlyPreviewerFS{ro, pv}, readOnlyStore{os}}
	case hasPv:
		return &readOnlyPreviewerFS{ro, pv}
	case hasOs:
		return &struct {
			*readOnlyFS
			readOnlyStore
		}{ro, readOnlyStore{os}}
	}
	return ro
}

type readOnlyStore struct{ os ObjectStore }

func (r readOnlyStore) NewCheck(size int64) Check               { return r.os.NewCheck(size) }
func (r readOnlyStore) Purge(_ context.Context, p string) error { return roErr("purge", p) }

type readOnlyPreviewerFS struct {
	*readOnlyFS
	pv Previewer
}

func (r *readOnlyPreviewerFS) Preview(ctx context.Context, p string, w, h int) (io.ReadCloser, string, error) {
	return r.pv.Preview(ctx, p, w, h)
}

type readOnlyFS struct{ FS }

func roErr(op, p string) error {
	return fmt.Errorf("files: %s %s: this drive is read-only: %w", op, p, odraz.ErrPermission)
}

func (r *readOnlyFS) Write(_ context.Context, p string, _ io.Reader, _ WriteOptions) error {
	return roErr("write", p)
}
func (r *readOnlyFS) Mkdir(_ context.Context, p string) error { return roErr("mkdir", p) }
func (r *readOnlyFS) Move(_ context.Context, from, _ string, _ bool) error {
	return roErr("move", from)
}
func (r *readOnlyFS) Delete(_ context.Context, p string, _ bool) error { return roErr("delete", p) }
func (r *readOnlyFS) Capabilities() Capabilities {
	c := r.FS.Capabilities()
	c.ReadOnly, c.AtomicMove, c.ServerSideCopy = true, false, false
	return c
}
