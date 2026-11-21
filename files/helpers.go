package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
)

// Upload writes r to path, creating missing parent folders first.
func Upload(ctx context.Context, fs FS, path string, r io.Reader, opts WriteOptions) error {
	path, err := CleanPath(path)
	if err != nil {
		return err
	}
	if dir, _ := Split(path); !IsRoot(dir) {
		if err := fs.Mkdir(ctx, dir); err != nil {
			return err
		}
	}
	return fs.Write(ctx, path, r, opts)
}

// UploadFile uploads a local file, taking the size from it.
func UploadFile(ctx context.Context, fs FS, remote, local string, opts WriteOptions) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	opts.Size = st.Size()
	return Upload(ctx, fs, remote, f, opts)
}

// DownloadOptions control Download.
type DownloadOptions struct {
	Offset   int64 // start reading here (resume)
	Progress func(done, total int64)
}

// Download streams a file into w starting at opts.Offset. Progress counts
// from the start of the file, so a resumed download reports offset+written.
func Download(ctx context.Context, fs FS, path string, w io.Writer, opts DownloadOptions) error {
	st, err := fs.Stat(ctx, path)
	if err != nil {
		return err
	}
	if st.IsDir {
		return fmt.Errorf("files: %s is a folder: %w", path, odraz.ErrInvalidPath)
	}
	return download(ctx, fs, path, w, opts.Offset, st.Size, opts.Progress)
}

func download(ctx context.Context, fs FS, path string, w io.Writer, off, total int64, progress func(done, total int64)) error {
	rc, err := fs.Read(ctx, path, off, -1)
	if err != nil {
		return err
	}
	defer rc.Close()
	done := off
	buf := make([]byte, 256<<10)
	for {
		n, rerr := rc.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			done += int64(n)
			if progress != nil {
				progress(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if done != total {
		return fmt.Errorf("files: %s: got %d of %d bytes: %w", path, done, total, io.ErrUnexpectedEOF)
	}
	return nil
}

// DownloadFile downloads to a local path with resume. Data goes to
// local+".part" and is renamed on completion. An existing .part file is
// continued only if it is not larger than the remote file and is newer than
// the remote modification time; otherwise the download restarts.
func DownloadFile(ctx context.Context, fs FS, remote, local string, opts DownloadOptions) error {
	st, err := fs.Stat(ctx, remote)
	if err != nil {
		return err
	}
	if st.IsDir {
		return fmt.Errorf("files: %s is a folder: %w", remote, odraz.ErrInvalidPath)
	}
	part := local + ".part"
	var off int64
	if fi, err := os.Stat(part); err == nil {
		if !st.ModTime.IsZero() && fi.Size() <= st.Size && fi.ModTime().After(st.ModTime) {
			off = fi.Size()
		} else {
			_ = os.Remove(part)
		}
	}
	flags := os.O_CREATE | os.O_WRONLY
	if off > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	if off < st.Size || st.Size == 0 {
		err = download(ctx, fs, remote, f, off, st.Size, opts.Progress)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err // keep .part for resume
	}
	return os.Rename(part, local)
}

// Rename changes the name of the item at path within its folder.
func Rename(ctx context.Context, fs FS, path, newName string, overwrite bool) error {
	path, err := CleanPath(path)
	if err != nil {
		return err
	}
	if newName == "" || strings.Contains(newName, "/") || newName == "." || newName == ".." {
		return fmt.Errorf("files: invalid name %q: %w", newName, odraz.ErrInvalidPath)
	}
	dir, _ := Split(path)
	to, err := Join(dir, newName)
	if err != nil {
		return err
	}
	return fs.Move(ctx, path, to, overwrite)
}

// SkipDir, returned by a Walk callback, skips the folder it was called for
// (or, for a file, the remaining entries of its folder).
var SkipDir = errors.New("skip this directory")

// Walk visits root and everything below it depth-first, children sorted by
// name. Returning SkipDir skips a folder; any other error stops the walk.
func Walk(ctx context.Context, fs FS, root string, fn func(Entry) error) error {
	root, err := CleanPath(root)
	if err != nil {
		return err
	}
	st, err := fs.Stat(ctx, root)
	if err != nil {
		return err
	}
	if err := walk(ctx, fs, st, fn); err != nil && !errors.Is(err, SkipDir) {
		return err
	}
	return nil
}

func walk(ctx context.Context, fs FS, e Entry, fn func(Entry) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fn(e); err != nil {
		return err
	}
	if !e.IsDir {
		return nil
	}
	kids, err := fs.List(ctx, e.Path)
	if err != nil {
		return err
	}
	sort.Slice(kids, func(i, j int) bool { return kids[i].Name < kids[j].Name })
	for _, k := range kids {
		if err := walk(ctx, fs, k, fn); err != nil {
			if errors.Is(err, SkipDir) {
				if k.IsDir {
					continue
				}
				return nil
			}
			return err
		}
	}
	return nil
}

// Copy copies a file or folder. It uses the backend's Copier when present
// and otherwise copies through the client (read + write, folders recursively).
// Without overwrite an existing target gives ErrExists; with overwrite an
// existing target is replaced. The target's parent must exist (ErrNotFound).
func Copy(ctx context.Context, fs FS, from, to string, overwrite bool) error {
	from, err := CleanPath(from)
	if err != nil {
		return err
	}
	to, err = CleanPath(to)
	if err != nil {
		return err
	}
	if IsRoot(to) || IsWithin(from, to) {
		return fmt.Errorf("files: cannot copy %s onto or into itself (%s): %w", from, to, odraz.ErrInvalidPath)
	}
	src, err := fs.Stat(ctx, from)
	if err != nil {
		return err
	}
	dir, _ := Split(to)
	if par, err := fs.Stat(ctx, dir); err != nil {
		return err
	} else if !par.IsDir {
		return fmt.Errorf("files: parent of %s is not a folder: %w", to, odraz.ErrNotFound)
	}
	if cp, ok := fs.(Copier); ok {
		return cp.Copy(ctx, from, to, overwrite)
	}
	if dst, err := fs.Stat(ctx, to); err == nil {
		if !overwrite {
			return fmt.Errorf("files: %s: %w", to, odraz.ErrExists)
		}
		if dst.IsDir != src.IsDir || dst.IsDir {
			if err := fs.Delete(ctx, to, true); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, odraz.ErrNotFound) {
		return err
	}
	return copyTree(ctx, fs, src, to)
}

func copyTree(ctx context.Context, fs FS, src Entry, to string) error {
	if !src.IsDir {
		rc, err := fs.Read(ctx, src.Path, 0, -1)
		if err != nil {
			return err
		}
		defer rc.Close()
		return fs.Write(ctx, to, rc, WriteOptions{Size: src.Size, Overwrite: true})
	}
	if err := fs.Mkdir(ctx, to); err != nil {
		return err
	}
	kids, err := fs.List(ctx, src.Path)
	if err != nil {
		return err
	}
	for _, k := range kids {
		if err := copyTree(ctx, fs, k, to+"/"+k.Name); err != nil {
			return err
		}
	}
	return nil
}
