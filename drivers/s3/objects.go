package s3

import (
	"context"
	"fmt"
	"net/http"

	"github.com/minio/minio-go/v7"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
)

// Purge removes the file at p with every old version and delete marker of
// exactly its key; for a folder that holds no file, those of its marker. A
// refused removal is an error.
func (f *fs) Purge(ctx context.Context, p string) error {
	if p == "/" {
		return fmt.Errorf("s3: cannot purge /: %w", odraz.ErrInvalidPath)
	}
	key := keyOf(f.prefix, p)
	n, err := f.purgeKey(ctx, key)
	if err != nil || n > 0 {
		return err // a file (if a key is both a file and a folder, the file wins)
	}
	// A folder: only an empty one goes.
	marker := key + "/"
	listing, cancel := context.WithCancel(ctx)
	defer cancel()
	for o := range f.mc.ListObjects(listing, f.bucket, minio.ListObjectsOptions{Prefix: marker, Recursive: true, MaxKeys: 2}) {
		if o.Err != nil {
			return mapErr(o.Err)
		}
		if o.Key != marker {
			return fmt.Errorf("s3: %s: %w", p, odraz.ErrNotEmpty)
		}
	}
	_, err = f.purgeKey(ctx, marker)
	return err
}

// purgeKey removes every version of exactly key and reports how many there
// were. On a server that cannot list versions it removes the object, if any.
func (f *fs) purgeKey(ctx context.Context, key string) (int, error) {
	listing, cancel := context.WithCancel(ctx)
	defer cancel()
	var ids []string
	// The prefix also matches longer keys, which sort after key itself.
	for o := range f.mc.ListObjects(listing, f.bucket, minio.ListObjectsOptions{Prefix: key, Recursive: true, WithVersions: true}) {
		if o.Err != nil {
			if minio.ToErrorResponse(o.Err).StatusCode == http.StatusNotImplemented {
				return f.removeUnversioned(ctx, key)
			}
			return 0, mapErr(o.Err)
		}
		if o.Key == key {
			ids = append(ids, o.VersionID)
		} else if o.Key > key {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := f.mc.RemoveObject(ctx, f.bucket, key, minio.RemoveObjectOptions{VersionID: id}); err != nil {
			return len(ids), mapErr(err)
		}
	}
	return len(ids), nil
}

func (f *fs) removeUnversioned(ctx context.Context, key string) (int, error) {
	if _, err := f.mc.StatObject(ctx, f.bucket, key, minio.StatObjectOptions{}); err != nil {
		if isNotFound(err) {
			return 0, nil
		}
		return 0, mapErr(err)
	}
	if err := f.mc.RemoveObject(ctx, f.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return 1, mapErr(err)
	}
	return 1, nil
}
