package files

import (
	"context"
	"io"
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
