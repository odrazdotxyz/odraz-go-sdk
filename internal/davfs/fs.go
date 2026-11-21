package davfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
	"github.com/odrazdotxyz/odraz-go-sdk/internal/dav"
)

// FS implements files.FS over a dav.Client. Shared by the generic WebDAV adapter
// and vendor adapters (OpenCloud) that expose WebDAV spaces. The
// public API stays files.FS.
type FS struct{ C *dav.Client }

var _ files.FS = (*FS)(nil)

func (f *FS) Capabilities() files.Capabilities {
	return files.Capabilities{AtomicMove: true, RealDirectories: true, RangeRead: true, ServerSideCopy: true}
}

func toEntry(r dav.Resource) files.Entry {
	_, name := files.Split(r.Path)
	e := files.Entry{Path: r.Path, Name: name, IsDir: r.IsDir, ModTime: r.ModTime, ETag: r.ETag, MimeType: r.ContentType}
	if !r.IsDir {
		e.Size = r.Size
	}
	return e
}

func (f *FS) Stat(ctx context.Context, p string) (files.Entry, error) {
	rs, err := f.C.Propfind(ctx, p, 0)
	if err != nil {
		return files.Entry{}, err
	}
	if len(rs) == 0 {
		return files.Entry{}, fmt.Errorf("webdav: %s: %w", p, odraz.ErrNotFound)
	}
	e := toEntry(rs[0])
	e.Path = p
	return e, nil
}

func (f *FS) List(ctx context.Context, p string) ([]files.Entry, error) {
	rs, err := f.C.Propfind(ctx, p, 1)
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 || !isSelfDir(rs, p) {
		return nil, fmt.Errorf("webdav: %s is not a folder: %w", p, odraz.ErrNotFound)
	}
	var out []files.Entry
	for _, r := range rs {
		if r.Path == p {
			continue
		}
		out = append(out, toEntry(r))
	}
	return out, nil
}

func isSelfDir(rs []dav.Resource, p string) bool {
	for _, r := range rs {
		if r.Path == p {
			return r.IsDir
		}
	}
	return rs[0].IsDir
}

func (f *FS) Read(ctx context.Context, p string, off, n int64) (io.ReadCloser, error) {
	h := http.Header{}
	switch {
	case n == 0:
		return io.NopCloser(eofReader{}), nil
	case n > 0:
		h.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	case off > 0:
		h.Set("Range", fmt.Sprintf("bytes=%d-", off))
	}
	resp, err := f.C.Do(ctx, dav.Request{Method: "GET", Path: p, Header: h})
	if err != nil {
		switch dav.Status(err) {
		case http.StatusRequestedRangeNotSatisfiable:
			return io.NopCloser(eofReader{}), nil // at or past EOF
		case http.StatusNotFound, http.StatusMethodNotAllowed:
			// Some servers answer a GET on a folder this way (Apache: 404).
			if f.isFolder(ctx, p) {
				return nil, folderErr(p)
			}
		}
		return nil, err
	}
	if resp.ContentLength == 0 {
		// OpenCloud answers a GET on a folder with an empty 200, which looks
		// like an empty file: confirm before returning.
		resp.Body.Close()
		if f.isFolder(ctx, p) {
			return nil, folderErr(p)
		}
		return io.NopCloser(eofReader{}), nil
	}
	if resp.StatusCode == http.StatusPartialContent || (off == 0 && n < 0) {
		return resp.Body, nil
	}
	// Server ignored the Range header: skip and limit locally.
	if _, err := io.CopyN(io.Discard, resp.Body, off); err != nil {
		resp.Body.Close()
		if errors.Is(err, io.EOF) {
			return io.NopCloser(eofReader{}), nil
		}
		return nil, err
	}
	if n < 0 {
		return resp.Body, nil
	}
	return &limited{io.LimitReader(resp.Body, n), resp.Body}, nil
}

func (f *FS) isFolder(ctx context.Context, p string) bool {
	st, err := f.Stat(ctx, p)
	return err == nil && st.IsDir
}

func folderErr(p string) error {
	return fmt.Errorf("webdav: %s is a folder, not a file: %w", p, odraz.ErrInvalidPath)
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

type limited struct {
	io.Reader
	c io.Closer
}

func (l *limited) Close() error { return l.c.Close() }

type countingReader struct {
	r        io.Reader
	done     int64
	total    int64
	progress func(done, total int64)
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if n > 0 {
		c.done += int64(n)
		if c.progress != nil {
			c.progress(c.done, c.total)
		}
	}
	return n, err
}

func (f *FS) Write(ctx context.Context, p string, r io.Reader, o files.WriteOptions) error {
	if cur, err := f.Stat(ctx, p); err == nil {
		if cur.IsDir || !o.Overwrite {
			return fmt.Errorf("webdav: %s: %w", p, odraz.ErrExists)
		}
	} else if !errors.Is(err, odraz.ErrNotFound) {
		return err
	}
	h := http.Header{}
	if !o.Overwrite {
		h.Set("If-None-Match", "*")
	}
	cr := &countingReader{r: r, total: o.Size, progress: o.Progress}
	req := dav.Request{Method: "PUT", Path: p, Header: h, Body: cr, Length: o.Size}
	if o.Size == 0 {
		req.Body, req.Length = nil, 0
	}
	resp, err := f.C.Do(ctx, req)
	if err != nil {
		switch dav.Status(err) {
		case http.StatusConflict:
			// Missing parent, or a folder in the way.
			if cur, serr := f.Stat(ctx, p); serr == nil && cur.IsDir {
				return fmt.Errorf("webdav: %s: %w", p, odraz.ErrExists)
			}
			return fmt.Errorf("webdav: parent of %s does not exist: %w: %w", p, odraz.ErrNotFound, err)
		case http.StatusMethodNotAllowed:
			return fmt.Errorf("webdav: %s: %w: %w", p, odraz.ErrExists, err)
		}
		return err
	}
	resp.Body.Close()
	if o.Progress != nil && (o.Size <= 0 || cr.done != o.Size) {
		o.Progress(cr.done, o.Size)
	}
	return nil
}

func (f *FS) Mkdir(ctx context.Context, p string) error {
	if files.IsRoot(p) {
		return nil
	}
	if cur, err := f.Stat(ctx, p); err == nil {
		if cur.IsDir {
			return nil
		}
		return fmt.Errorf("webdav: %s is a file: %w", p, odraz.ErrExists)
	} else if !errors.Is(err, odraz.ErrNotFound) {
		return err
	}
	// MKCOL creates one level only: walk from the top.
	cur := ""
	for _, seg := range splitSegments(p) {
		cur += "/" + seg
		if err := f.mkcol(ctx, cur); err != nil {
			return err
		}
	}
	return nil
}

func (f *FS) mkcol(ctx context.Context, p string) error {
	resp, err := f.C.Do(ctx, dav.Request{Method: "MKCOL", Path: p})
	if err == nil {
		resp.Body.Close()
		return nil
	}
	if dav.Status(err) == http.StatusMethodNotAllowed { // already exists
		cur, serr := f.Stat(ctx, p)
		if serr != nil {
			return serr
		}
		if cur.IsDir {
			return nil
		}
		return fmt.Errorf("webdav: %s is a file: %w: %w", p, odraz.ErrExists, err)
	}
	return err
}

func splitSegments(p string) []string {
	var out []string
	start := 1
	for i := 1; i <= len(p); i++ {
		if i == len(p) || p[i] == '/' {
			if i > start {
				out = append(out, p[start:i])
			}
			start = i + 1
		}
	}
	return out
}

func (f *FS) Move(ctx context.Context, from, to string, overwrite bool) error {
	if _, err := f.Stat(ctx, from); err != nil {
		return err
	}
	// Some servers answer 500 for a missing destination parent: check first.
	dir, _ := files.Split(to)
	if par, err := f.Stat(ctx, dir); err != nil {
		return err
	} else if !par.IsDir {
		return fmt.Errorf("webdav: parent of %s is not a folder: %w", to, odraz.ErrNotFound)
	}
	h := http.Header{}
	h.Set("Destination", f.C.URL(to))
	if overwrite {
		h.Set("Overwrite", "T")
	} else {
		h.Set("Overwrite", "F")
	}
	resp, err := f.C.Do(ctx, dav.Request{Method: "MOVE", Path: from, Header: h})
	if err != nil {
		if dav.Status(err) == http.StatusConflict {
			return fmt.Errorf("webdav: cannot move %s to %s: %w: %w", from, to, odraz.ErrNotFound, err)
		}
		return err
	}
	resp.Body.Close()
	return nil
}

func (f *FS) Delete(ctx context.Context, p string, recursive bool) error {
	cur, err := f.Stat(ctx, p)
	if err != nil {
		return err
	}
	if cur.IsDir && !recursive {
		kids, err := f.List(ctx, p)
		if err != nil {
			return err
		}
		if len(kids) > 0 {
			return fmt.Errorf("webdav: %s: %w", p, odraz.ErrNotEmpty)
		}
	}
	resp, err := f.C.Do(ctx, dav.Request{Method: "DELETE", Path: p})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

var _ files.Copier = (*FS)(nil)

// Copy uses the WebDAV COPY method (server side).
func (f *FS) Copy(ctx context.Context, from, to string, overwrite bool) error {
	if _, err := f.Stat(ctx, from); err != nil {
		return err
	}
	dir, _ := files.Split(to)
	if par, err := f.Stat(ctx, dir); err != nil {
		return err
	} else if !par.IsDir {
		return fmt.Errorf("webdav: parent of %s is not a folder: %w", to, odraz.ErrNotFound)
	}
	h := http.Header{}
	h.Set("Destination", f.C.URL(to))
	h.Set("Overwrite", map[bool]string{true: "T", false: "F"}[overwrite])
	resp, err := f.C.Do(ctx, dav.Request{Method: "COPY", Path: from, Header: h})
	if err != nil {
		if dav.Status(err) == http.StatusConflict {
			return fmt.Errorf("webdav: cannot copy %s to %s: %w: %w", from, to, odraz.ErrNotFound, err)
		}
		return err
	}
	resp.Body.Close()
	return nil
}
