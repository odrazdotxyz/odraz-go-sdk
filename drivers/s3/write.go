package s3

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
)

// shaMetaKey is the user metadata key (stored by S3 as X-Amz-Meta-Odraz-Sha256)
// that holds "<64 lowercase hex SHA-256>:<size in bytes>" of the whole object.
// The size binds the hash to its bytes a little: SHA256 refuses the value when
// the object's current size differs (another tool rewrote the content but kept
// the metadata). Same-length corruption or replacement is NOT detected: the hash
// only vouches for what this driver wrote, and a caller that must be sure
// has to verify by reading the object. S3's own SHA-256
// checksum of a multipart upload is only a composite (a hash of the part
// hashes), a single-part one needs trailing headers that not every server
// accepts, and the hash of a stream is only known at its end, so Write always
// records the full-object hash here.
const shaMetaKey = "odraz-sha256"

// unknownSizePart is the part size of an upload of unknown length. minio-go
// would size the buffer for the largest possible object (hundreds of MiB), so
// it is set here: 8 MiB parts allow about 78 GiB, and the memory in use is one
// part. Uploads of known size keep the library's choice.
const unknownSizePart = 8 << 20

// maxUnknownParts is the part limit of S3 multipart uploads.
const maxUnknownParts = 10000

// Variables so tests can shrink them; production code never changes them.
var (
	unknownPartSize int64 = unknownSizePart
	maxUnknownSize        = unknownPartSize * maxUnknownParts // largest stream of unknown length
)

// abortTimeout bounds the best-effort cleanup of a failed upload.
const abortTimeout = 30 * time.Second

// smallObject is the largest known size that Write reads into memory to hash
// it before uploading.
const smallObject = 8 << 20

// copyWorkers is how many objects a folder copy or move copies at once.
const copyWorkers = 8

// hashReader hashes what passes through and reports progress. minio-go reads
// the source once and sequentially, so the hash is that of the whole object.
type hashReader struct {
	r        io.Reader
	h        hash.Hash
	n        int64
	total    int64
	progress func(done, total int64)
}

func (h *hashReader) Read(b []byte) (int, error) {
	n, err := h.r.Read(b)
	if n > 0 {
		h.h.Write(b[:n])
		h.n += int64(n)
		if h.progress != nil {
			h.progress(h.n, h.total)
		}
	}
	return n, err
}

// conditionalPut reports whether the server honours "If-None-Match: *" on
// PUT, found out once per drive by trying it on a throwaway key: a second
// conditional PUT must fail with 412. A server that answers 501 or ignores the
// header (many S3 clones) reports false.
func (f *fs) conditionalPut(ctx context.Context) bool {
	f.condOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		var rnd [8]byte
		_, _ = crand.Read(rnd[:])
		key := f.prefix + ".odraz-probe-" + hex.EncodeToString(rnd[:])
		if _, err := f.mc.PutObject(ctx, f.bucket, key, bytes.NewReader([]byte{0}), 1, minio.PutObjectOptions{}); err != nil {
			return
		}
		defer f.mc.RemoveObject(ctx, f.bucket, key, minio.RemoveObjectOptions{})
		o := minio.PutObjectOptions{}
		o.SetMatchETagExcept("*")
		_, err := f.mc.PutObject(ctx, f.bucket, key, bytes.NewReader([]byte{0}), 1, o)
		f.condPut = minio.ToErrorResponse(err).StatusCode == http.StatusPreconditionFailed
	})
	return f.condPut
}

// normMeta lowercases caller metadata keys, stripping a leading "x-amz-meta-"
// (any case) so a caller cannot reach the S3 header of another key by its
// full name. The reserved key of the stored hash is dropped, whatever its
// spelling: callers cannot set it.
func normMeta(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for k, v := range m {
		k = strings.ToLower(k)
		k = strings.TrimPrefix(k, "x-amz-meta-")
		if k == shaMetaKey {
			continue
		}
		out[k] = v
	}
	return out
}

// abortUpload removes incomplete multipart uploads of key, best effort. The
// client library aborts a failed upload with the caller's context, which fails
// exactly when the failure was a cancelled context, and a multipart copy
// never aborts, so unfinished parts would stay (and be billed). It removes
// every incomplete upload of the key, also one of a concurrent writer.
func (f *fs) abortUpload(ctx context.Context, key string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortTimeout)
	defer cancel()
	_ = f.mc.RemoveIncompleteUpload(ctx, f.bucket, key)
}

// removeAfterFailure deletes an object a failed Write left behind.
func (f *fs) removeAfterFailure(ctx context.Context, key string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortTimeout)
	defer cancel()
	_ = f.mc.RemoveObject(ctx, f.bucket, key, minio.RemoveObjectOptions{})
}

// moreData reports whether r still has bytes (a source longer than declared).
func moreData(r io.Reader) (bool, error) {
	var b [1]byte
	n, err := io.ReadFull(r, b[:])
	if n > 0 {
		return true, nil
	}
	if err == io.EOF {
		return false, nil
	}
	return false, err
}

// requireParent returns ErrNotFound unless the parent folder of p exists (the
// root always does).
func (f *fs) requireParent(ctx context.Context, p string) error {
	parent, _ := files.Split(p)
	if parent == "/" {
		return nil
	}
	ok, _, err := f.folder(ctx, parent)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("s3: %s: parent folder missing: %w", p, odraz.ErrNotFound)
	}
	return nil
}

func (f *fs) Write(ctx context.Context, p string, r io.Reader, opts files.WriteOptions) error {
	if p == "/" {
		return fmt.Errorf("s3: cannot write to /: %w", odraz.ErrInvalidPath)
	}
	if err := f.requireParent(ctx, p); err != nil {
		return err
	}
	key := keyOf(f.prefix, p)
	cond := false
	existing, err := f.Stat(ctx, p)
	switch {
	case err == nil:
		// A folder in the way is ErrExists even with Overwrite (fstest contract).
		if !opts.Overwrite || existing.IsDir {
			return fmt.Errorf("s3: %s: %w", p, odraz.ErrExists)
		}
	case errors.Is(err, odraz.ErrNotFound):
		// Not there yet. Where the server supports it a single-PUT upload
		// (known size up to the size minio-go sends in one request, 16 MiB)
		// also carries If-None-Match: * and a writer that got in between makes
		// it fail with 412 (ErrExists). Multipart and unknown-size uploads
		// are NOT conditional (the library cannot make the completion
		// conditional): only the Stat above guards them, and two concurrent
		// writers of one key can both succeed. The same holds on servers that
		// ignore the header.
		cond = !opts.Overwrite && f.conditionalPut(ctx)
	default:
		return err
	}

	meta := normMeta(opts.Metadata)
	po := minio.PutObjectOptions{UserMetadata: meta, ContentType: mime.TypeByExtension(path.Ext(p))}
	if cond {
		po.SetMatchETagExcept("*")
	}
	hr := &hashReader{r: r, h: sha256.New(), total: opts.Size, progress: opts.Progress}

	if opts.Size >= 0 && opts.Size <= smallObject {
		// Small: read it (at most 8 MiB) so the hash is known before the
		// request, and store it in the same PUT.
		buf := make([]byte, opts.Size)
		if _, err := io.ReadFull(hr, buf); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				return fmt.Errorf("s3: %s: source ended before %d bytes: %w", p, opts.Size, io.ErrUnexpectedEOF)
			}
			return err
		}
		if more, err := moreData(r); err != nil {
			return err
		} else if more {
			return fmt.Errorf("s3: %s: source is longer than the declared %d bytes: %w", p, opts.Size, odraz.ErrInvalidPath)
		}
		meta[shaMetaKey] = hashValue(hr.h.Sum(nil), opts.Size)
		_, err := f.mc.PutObject(ctx, f.bucket, key, bytes.NewReader(buf), opts.Size, po)
		return mapErr(err)
	}

	// Large or unknown size: stream it, hashing on the way. The hash is only
	// known at the end, and user metadata can only be set at the start of an
	// upload, so it is added afterwards by copying the object onto itself.
	// The copy is server-side but its cost grows with the object.
	src := io.Reader(hr)
	if opts.Size < 0 {
		po.PartSize = uint64(unknownPartSize)
		// Never hand the library more than it can upload, so that what is
		// left over is seen below instead of being cut off silently.
		hr.r = io.LimitReader(r, maxUnknownSize)
	} else {
		// Likewise for a source longer than declared: the library would
		// fail half way; let it see exactly Size bytes and check below.
		hr.r = io.LimitReader(r, opts.Size)
	}
	ui, err := f.mc.PutObject(ctx, f.bucket, key, src, opts.Size, po)
	if err != nil {
		f.abortUpload(ctx, key)
		return mapErr(err)
	}
	// A source longer than declared (known size) or than the cap (unknown
	// size) would otherwise be stored cut off, with a hash of the cut-off
	// bytes. Fail and remove the object instead.
	if more, merr := moreData(r); merr != nil || more {
		f.removeAfterFailure(ctx, key)
		if merr != nil {
			return merr
		}
		if opts.Size < 0 {
			return fmt.Errorf("s3: %s: stream exceeds the maximum supported size of %d bytes: %w", p, maxUnknownSize, odraz.ErrUnsupported)
		}
		return fmt.Errorf("s3: %s: source is longer than the declared %d bytes: %w", p, opts.Size, odraz.ErrInvalidPath)
	}
	md := make(map[string]string, len(meta)+2)
	for k, v := range meta {
		md[k] = v
	}
	md[shaMetaKey] = hashValue(hr.h.Sum(nil), hr.n)
	if po.ContentType != "" {
		md["Content-Type"] = po.ContentType
	}
	// Conditional on the ETag of the upload: it fails instead of touching an
	// object that someone else replaced in between.
	_, err = f.mc.ComposeObject(ctx,
		minio.CopyDestOptions{Bucket: f.bucket, Object: key, UserMetadata: md, ReplaceMetadata: true},
		minio.CopySrcOptions{Bucket: f.bucket, Object: key, MatchETag: strings.Trim(ui.ETag, `"`)})
	if err != nil {
		f.abortUpload(ctx, key) // a multipart copy (above 5 GiB) is never aborted by the library
		return mapErr(err)
	}
	return nil
}

func hashValue(sum []byte, size int64) string {
	return hex.EncodeToString(sum) + ":" + strconv.FormatInt(size, 10)
}

// parseHashValue splits "<64 lowercase hex>:<decimal size>".
func parseHashValue(v string) (hexSum string, size int64, ok bool) {
	h, sz, found := strings.Cut(v, ":")
	if !found || len(h) != 2*sha256.Size || sz == "" || len(sz) > 19 {
		return "", 0, false
	}
	for _, c := range h {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", 0, false
		}
	}
	for _, c := range sz {
		if c < '0' || c > '9' {
			return "", 0, false
		}
	}
	n, err := strconv.ParseInt(sz, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return h, n, true
}

// SHA256 returns the hash of the whole object: the user metadata Write
// recorded, else a full-object S3 SHA-256 checksum that another tool stored.
// Objects with neither give ErrUnsupported.
func (f *fs) SHA256(ctx context.Context, p string) (string, error) {
	st, err := f.mc.StatObject(ctx, f.bucket, keyOf(f.prefix, p), minio.StatObjectOptions{Checksum: true})
	if err != nil {
		if !isNotFound(err) {
			return "", mapErr(err)
		}
		e, serr := f.Stat(ctx, p)
		if serr != nil {
			return "", serr
		}
		if e.IsDir {
			return "", fmt.Errorf("s3: %s is a folder: %w", p, odraz.ErrInvalidPath)
		}
		return "", fmt.Errorf("s3: %s: %w", p, odraz.ErrNotFound)
	}
	for k, v := range st.UserMetadata {
		if strings.EqualFold(k, shaMetaKey) {
			h, size, ok := parseHashValue(v)
			if !ok {
				return "", fmt.Errorf("s3: %s: stored SHA-256 is malformed: %w", p, odraz.ErrUnsupported)
			}
			if size != st.Size {
				return "", fmt.Errorf("s3: %s: stored SHA-256 is for %d bytes but the object has %d: %w", p, size, st.Size, odraz.ErrUnsupported)
			}
			return h, nil
		}
	}
	// A composite checksum ("<base64>-<parts>") is not the object's hash.
	if cs := st.ChecksumSHA256; cs != "" && !strings.Contains(cs, "-") {
		if b, err := base64.StdEncoding.DecodeString(cs); err == nil && len(b) == sha256.Size {
			return hex.EncodeToString(b), nil
		}
	}
	return "", fmt.Errorf("s3: %s has no stored SHA-256: %w", p, odraz.ErrUnsupported)
}

func (f *fs) Mkdir(ctx context.Context, p string) error {
	if p == "/" {
		return nil
	}
	cur := ""
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		cur += "/" + seg
		key := keyOf(f.prefix, cur)
		if _, err := f.mc.StatObject(ctx, f.bucket, key, minio.StatObjectOptions{}); err == nil {
			return fmt.Errorf("s3: %s is a file: %w", cur, odraz.ErrExists)
		} else if !isNotFound(err) {
			return mapErr(err)
		}
		ok, _, err := f.folder(ctx, cur)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		if _, err := f.mc.PutObject(ctx, f.bucket, key+"/", bytes.NewReader(nil), 0, minio.PutObjectOptions{}); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

// keysUnder lists every key below the folder p, markers included.
func (f *fs) keysUnder(ctx context.Context, p string) ([]string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var keys []string
	for o := range f.mc.ListObjects(ctx, f.bucket, minio.ListObjectsOptions{Prefix: f.dirKey(p), Recursive: true}) {
		if o.Err != nil {
			return nil, mapErr(o.Err)
		}
		keys = append(keys, o.Key)
	}
	return keys, nil
}

// dirKey is the key prefix of the contents of folder p.
func (f *fs) dirKey(p string) string {
	if p == "/" {
		return f.prefix
	}
	return keyOf(f.prefix, p) + "/"
}

// removeKeys deletes keys in batches (RemoveObjects sends up to 1,000 per
// request).
func (f *fs) removeKeys(ctx context.Context, keys []string) error {
	ch := make(chan minio.ObjectInfo)
	go func() {
		defer close(ch)
		for _, k := range keys {
			select {
			case ch <- minio.ObjectInfo{Key: k}:
			case <-ctx.Done():
				return
			}
		}
	}()
	var first error
	for e := range f.mc.RemoveObjects(ctx, f.bucket, ch, minio.RemoveObjectsOptions{}) {
		if e.Err != nil && first == nil {
			first = mapErr(e.Err)
		}
	}
	if first == nil {
		first = ctx.Err()
	}
	return first
}

func (f *fs) Delete(ctx context.Context, p string, recursive bool) error {
	if p != "/" {
		// The exact key wins over a folder of the same name.
		_, err := f.mc.StatObject(ctx, f.bucket, keyOf(f.prefix, p), minio.StatObjectOptions{})
		if err == nil {
			return mapErr(f.mc.RemoveObject(ctx, f.bucket, keyOf(f.prefix, p), minio.RemoveObjectOptions{}))
		}
		if !isNotFound(err) {
			return mapErr(err)
		}
	}
	keys, err := f.keysUnder(ctx, p)
	if err != nil {
		return err
	}
	if len(keys) == 0 && p != "/" {
		return fmt.Errorf("s3: %s: %w", p, odraz.ErrNotFound)
	}
	if !recursive {
		for _, k := range keys {
			if k != f.dirKey(p) {
				return fmt.Errorf("s3: %s: %w", p, odraz.ErrNotEmpty)
			}
		}
	}
	return f.removeKeys(ctx, keys)
}

func (f *fs) Move(ctx context.Context, from, to string, overwrite bool) error {
	return f.transfer(ctx, from, to, overwrite, true)
}

// Copy is a server-side copy; a folder is copied key by key.
func (f *fs) Copy(ctx context.Context, from, to string, overwrite bool) error {
	return f.transfer(ctx, from, to, overwrite, false)
}

// transfer copies from to to, then deletes the source if move is set. S3 has
// no rename, so a move is not atomic: if it fails half way, both the copied
// and the source keys can exist. Without overwrite the target's absence is
// checked first, which a concurrent writer can race.
func (f *fs) transfer(ctx context.Context, from, to string, overwrite, move bool) error {
	if from == "/" || to == "/" {
		return fmt.Errorf("s3: cannot copy or move the root: %w", odraz.ErrInvalidPath)
	}
	src, err := f.Stat(ctx, from)
	if err != nil {
		return err
	}
	if from == to {
		if move || overwrite {
			return nil
		}
		return fmt.Errorf("s3: %s: %w", to, odraz.ErrExists)
	}
	if strings.HasPrefix(to, from+"/") || strings.HasPrefix(from, to+"/") {
		return fmt.Errorf("s3: %s and %s are nested: %w", from, to, odraz.ErrInvalidPath)
	}
	if err := f.requireParent(ctx, to); err != nil {
		return err
	}
	dst, err := f.Stat(ctx, to)
	switch {
	case err == nil:
		if !overwrite {
			return fmt.Errorf("s3: %s: %w", to, odraz.ErrExists)
		}
		if src.IsDir || dst.IsDir {
			// A folder replaces or is replaced: remove the target first.
			if err := f.Delete(ctx, to, true); err != nil {
				return err
			}
		}
	case !errors.Is(err, odraz.ErrNotFound):
		return err
	}

	if !src.IsDir {
		if err := f.copyKey(ctx, keyOf(f.prefix, from), keyOf(f.prefix, to)); err != nil {
			return err
		}
		if move {
			return mapErr(f.mc.RemoveObject(ctx, f.bucket, keyOf(f.prefix, from), minio.RemoveObjectOptions{}))
		}
		return nil
	}

	keys, err := f.keysUnder(ctx, from)
	if err != nil {
		return err
	}
	srcDir, dstDir := f.dirKey(from), f.dirKey(to)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sem      = make(chan struct{}, copyWorkers)
	)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, k := range keys {
		sem <- struct{}{}
		if cctx.Err() != nil {
			<-sem
			break
		}
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := f.copyKey(cctx, k, dstDir+strings.TrimPrefix(k, srcDir)); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
			}
		}(k)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if move {
		return f.removeKeys(ctx, keys)
	}
	return nil
}

// copyKey copies one object on the server, with its metadata. A folder
// marker has no content to copy and is created again.
func (f *fs) copyKey(ctx context.Context, src, dst string) error {
	if strings.HasSuffix(src, "/") {
		_, err := f.mc.PutObject(ctx, f.bucket, dst, bytes.NewReader(nil), 0, minio.PutObjectOptions{})
		return mapErr(err)
	}
	_, err := f.mc.ComposeObject(ctx,
		minio.CopyDestOptions{Bucket: f.bucket, Object: dst},
		minio.CopySrcOptions{Bucket: f.bucket, Object: src})
	if err != nil {
		f.abortUpload(ctx, dst) // a multipart copy (above 5 GiB) is never aborted by the library
	}
	return mapErr(err)
}
