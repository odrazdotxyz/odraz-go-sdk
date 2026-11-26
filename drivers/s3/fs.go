package s3

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
)

// fs is the files.FS of one account: a bucket, or a prefix inside it.
//
// Folders are virtual. A path is a folder if the marker key "<key>/" exists
// or any key starts with "<key>/"; it is a file if the exact key exists. When
// both hold, the exact key wins: Stat returns the file and List returns
// ErrInvalidPath.
type fs struct {
	mc     *minio.Client
	bucket string
	prefix string // key prefix with trailing slash, or ""

	condOnce sync.Once
	condPut  bool // the server honours If-None-Match: * (see conditionalPut)

	verOnce sync.Once
	phantom bool // listings may show folders that no longer exist (see keepsVersions)
}

func isNotFound(err error) bool {
	r := minio.ToErrorResponse(err)
	return r.StatusCode == http.StatusNotFound || r.Code == "NoSuchKey" || r.Code == "NotFound"
}

func fileEntry(p string, o minio.ObjectInfo) files.Entry {
	_, name := files.Split(p)
	return files.Entry{Path: p, Name: name, Size: o.Size, ModTime: o.LastModified, ETag: o.ETag, MimeType: o.ContentType}
}

func dirEntry(p string, mod time.Time) files.Entry {
	_, name := files.Split(p)
	return files.Entry{Path: p, Name: name, IsDir: true, ModTime: mod}
}

// folder reports whether p has a marker or any key below it. The marker's
// time is returned when there is one.
func (f *fs) folder(ctx context.Context, p string) (bool, time.Time, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // stop the listing after the first object
	marker := keyOf(f.prefix, p) + "/"
	// Recursive, so that only current objects count: a listing by folder can show
	// one in a versioned bucket (MinIO does) that holds only deleted versions.
	for o := range f.mc.ListObjects(ctx, f.bucket, minio.ListObjectsOptions{Prefix: marker, Recursive: true, MaxKeys: 1}) {
		if o.Err != nil {
			return false, time.Time{}, mapErr(o.Err)
		}
		if o.Key == marker {
			return true, o.LastModified, nil
		}
		return true, time.Time{}, nil
	}
	return false, time.Time{}, nil
}

// keepsVersions reports, found out once per drive, whether the bucket keeps old
// versions, so that List has to confirm the folders it is shown. When that
// cannot be asked (a restricted key) it assumes so; a server without
// versioning (it answers 501) does not.
func (f *fs) keepsVersions(ctx context.Context) bool {
	f.verOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		v, err := f.mc.GetBucketVersioning(ctx, f.bucket)
		switch {
		case err == nil:
			f.phantom = v.Status != ""
		default:
			f.phantom = minio.ToErrorResponse(err).StatusCode != http.StatusNotImplemented
		}
	})
	return f.phantom
}

// dropEmpty removes from entries the folders that hold no current object.
func (f *fs) dropEmpty(ctx context.Context, entries map[string]files.Entry) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		slots = make(chan struct{}, copyWorkers)
		gone  []string
	)
	for p, e := range entries {
		if !e.IsDir {
			continue
		}
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			ok, _, err := f.folder(ctx, p)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil && first == nil:
				first = err
				cancel()
			case err == nil && !ok:
				gone = append(gone, p)
			}
		}()
	}
	wg.Wait()
	for _, p := range gone {
		delete(entries, p)
	}
	return first
}

func (f *fs) Stat(ctx context.Context, p string) (files.Entry, error) {
	if p == "/" {
		return dirEntry("/", time.Time{}), nil
	}
	o, err := f.mc.StatObject(ctx, f.bucket, keyOf(f.prefix, p), minio.StatObjectOptions{})
	if err == nil {
		return fileEntry(p, o), nil
	}
	if !isNotFound(err) {
		return files.Entry{}, mapErr(err)
	}
	ok, mod, err := f.folder(ctx, p)
	if err != nil {
		return files.Entry{}, err
	}
	if !ok {
		return files.Entry{}, fmt.Errorf("s3: %s: %w", p, odraz.ErrNotFound)
	}
	return dirEntry(p, mod), nil
}

func (f *fs) List(ctx context.Context, p string) ([]files.Entry, error) {
	if p != "/" {
		if _, err := f.mc.StatObject(ctx, f.bucket, keyOf(f.prefix, p), minio.StatObjectOptions{}); err == nil {
			return nil, fmt.Errorf("s3: %s is a file: %w", p, odraz.ErrInvalidPath)
		} else if !isNotFound(err) {
			return nil, mapErr(err)
		}
	}
	dir := keyOf(f.prefix, p)
	if p != "/" {
		dir += "/"
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	byPath := map[string]files.Entry{}
	exists := p == "/" // the root always exists
	// ListObjects pages through the bucket by itself until the listing ends.
	for o := range f.mc.ListObjects(ctx, f.bucket, minio.ListObjectsOptions{Prefix: dir}) {
		if o.Err != nil {
			return nil, mapErr(o.Err)
		}
		exists = true
		if o.Key == dir {
			continue // the folder's own marker
		}
		cp := pathOf(f.prefix, o.Key)
		if strings.HasSuffix(o.Key, "/") {
			// A common prefix (or a marker folded into one). Its time is not
			// known without another request.
			if cp == "/" || strings.HasSuffix(cp, "/") {
				continue // a key with an empty name segment
			}
			if _, ok := byPath[cp]; !ok { // a file of the same name wins
				byPath[cp] = dirEntry(cp, time.Time{})
			}
			continue
		}
		if strings.HasSuffix(cp, "/") || strings.Contains(strings.TrimPrefix(o.Key, dir), "/") {
			continue
		}
		byPath[cp] = fileEntry(cp, o)
	}
	if len(byPath) > 0 && f.keepsVersions(ctx) {
		had := len(byPath)
		if err := f.dropEmpty(ctx, byPath); err != nil {
			return nil, err
		}
		if len(byPath) < had && len(byPath) == 0 && p != "/" {
			// Only folders that are gone: is this one still there?
			ok, _, err := f.folder(ctx, p)
			if err != nil {
				return nil, err
			}
			exists = ok
		}
	}
	if !exists {
		return nil, fmt.Errorf("s3: %s: %w", p, odraz.ErrNotFound)
	}
	out := make([]files.Entry, 0, len(byPath))
	for _, e := range byPath {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (f *fs) Read(ctx context.Context, p string, offset, length int64) (io.ReadCloser, error) {
	key := keyOf(f.prefix, p)
	if p == "/" {
		return nil, fmt.Errorf("s3: / is a folder: %w", odraz.ErrInvalidPath)
	}
	opts := minio.GetObjectOptions{}
	switch {
	case length == 0:
		e, err := f.Stat(ctx, p)
		if err != nil {
			return nil, err
		}
		if e.IsDir {
			return nil, fmt.Errorf("s3: %s is a folder: %w", p, odraz.ErrInvalidPath)
		}
		return io.NopCloser(strings.NewReader("")), nil
	case length > 0:
		if err := opts.SetRange(offset, offset+length-1); err != nil {
			return nil, fmt.Errorf("s3: bad range: %w", odraz.ErrInvalidPath)
		}
	case offset > 0:
		if err := opts.SetRange(offset, 0); err != nil {
			return nil, fmt.Errorf("s3: bad range: %w", odraz.ErrInvalidPath)
		}
	}
	// GetObject is lazy and a failed first request would only show up at the
	// first Read, so check the object first: it separates a missing file from
	// a folder, and the caller gets the error from Read itself.
	if _, err := f.mc.StatObject(ctx, f.bucket, key, minio.StatObjectOptions{}); err != nil {
		if !isNotFound(err) {
			return nil, mapErr(err)
		}
		ok, _, ferr := f.folder(ctx, p)
		if ferr != nil {
			return nil, ferr
		}
		if ok {
			return nil, fmt.Errorf("s3: %s is a folder: %w", p, odraz.ErrInvalidPath)
		}
		return nil, fmt.Errorf("s3: %s: %w", p, odraz.ErrNotFound)
	}
	obj, err := f.mc.GetObject(ctx, f.bucket, key, opts)
	if err != nil {
		return nil, mapErr(err)
	}
	return mappedReader{obj}, nil
}

// mappedReader maps S3 errors that surface while the body is read.
type mappedReader struct{ *minio.Object }

func (r mappedReader) Read(b []byte) (int, error) {
	n, err := r.Object.Read(b)
	if err != nil && err != io.EOF {
		err = mapErr(err)
	}
	return n, err
}

func (f *fs) Capabilities() files.Capabilities {
	return files.Capabilities{RangeRead: true, ServerSideCopy: true}
}
