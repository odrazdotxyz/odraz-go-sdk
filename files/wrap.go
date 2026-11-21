package files

import (
	"context"
	"fmt"
	"io"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
)

// Wrap returns an FS that normalizes every path with CleanPath before
// calling inner, so adapters never see unclean or root-escaping paths.
// Optional interfaces of inner (currently Copier) are preserved.
func Wrap(inner FS) FS {
	c := &cleanFS{inner}
	if cp, ok := inner.(Copier); ok {
		return &cleanCopierFS{c, cp}
	}
	return c
}

type cleanCopierFS struct {
	*cleanFS
	cp Copier
}

func (c *cleanCopierFS) Copy(ctx context.Context, from, to string, ow bool) error {
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
// ReadOnly. Optional write interfaces (Copier) are not exposed.
func ReadOnly(inner FS) FS { return &readOnlyFS{inner} }

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
