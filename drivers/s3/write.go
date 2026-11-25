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
	"net/http"
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

// singlePutLimit is the largest known size sent as one PUT request. It is set
// as the part size of such an upload, so that this driver, not the library,
// decides which uploads are a single PUT (and may carry If-None-Match).
const singlePutLimit = 16 << 20

// abortTimeout bounds the best-effort cleanup of a failed upload.
const abortTimeout = 30 * time.Second

// smallObject is the largest known size that Write reads into memory to hash
// it before uploading.
const smallObject = 8 << 20

// maxCopyObject is the largest object S3 copies in one CopyObject request.
// A variable so tests can shrink it; production code never changes it.
var maxCopyObject int64 = 5 << 30

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

// probeKey starts the name of the throwaway object of conditionalPut, in a
// folder of its own so that it does not show among the user's files (the folder
// is virtual and goes with the object; both stay only if deleting fails).
const probeKey = ".odraz/probe-"

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
		key := f.prefix + probeKey + hex.EncodeToString(rnd[:])
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

// abortUpload removes incomplete multipart uploads of key, best effort. The
// client library aborts a failed upload with the caller's context, which fails
// exactly when the failure was a cancelled context, and a multipart copy (used
// above maxCopyObject) never aborts, so unfinished parts would stay (and be
// billed). It removes every incomplete upload of the
// key, also one of a concurrent writer.
func (f *fs) abortUpload(ctx context.Context, key string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortTimeout)
	defer cancel()
	_ = f.mc.RemoveIncompleteUpload(ctx, f.bucket, key)
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
	// Bad metadata fails before any request and before the source is touched.
	po, err := putOptions(p, opts.Metadata)
	if err != nil {
		return err
	}
	if err := f.requireParent(ctx, p); err != nil {
		return err
	}
	// The metadata of the second copy (below), taken before the library gets
	// po: it adds entries of its own to the map.
	md := replacementMetadata(po)
	key := keyOf(f.prefix, p)
	single := opts.Size >= 0 && opts.Size <= singlePutLimit
	cond := false
	existing, err := f.Stat(ctx, p)
	switch {
	case err == nil:
		// A folder in the way is ErrExists even with Overwrite (fstest contract).
		if !opts.Overwrite || existing.IsDir {
			return fmt.Errorf("s3: %s: %w", p, odraz.ErrExists)
		}
	case errors.Is(err, odraz.ErrNotFound):
		// Not there yet. Where the server supports it a single-PUT upload also
		// carries If-None-Match: *, so a writer that got in between makes it fail
		// with 412 (ErrExists). A multipart upload does not send the header: the
		// high-level PutObject can only send it when the upload is initiated, not
		// on completion (where AWS documents it), so only the Stat above guards
		// it, as it does every upload on servers that ignore the header.
		cond = !opts.Overwrite && single && f.conditionalPut(ctx)
	default:
		return err
	}

	if single {
		po.PartSize = singlePutLimit
	}
	if cond {
		po.SetMatchETagExcept("*")
	}
	var lim *strictReader
	if opts.Size < 0 {
		po.PartSize = uint64(unknownPartSize)
		lim = newStrictReader(r, maxUnknownSize,
			fmt.Errorf("s3: %s: stream exceeds the maximum supported size of %d bytes: %w", p, maxUnknownSize, odraz.ErrUnsupported))
	} else {
		lim = newStrictReader(r, opts.Size,
			fmt.Errorf("s3: %s: source is longer than the declared %d bytes: %w", p, opts.Size, odraz.ErrInvalidPath))
	}
	hr := &hashReader{r: lim, h: sha256.New(), total: opts.Size, progress: opts.Progress}

	if opts.Size >= 0 && opts.Size <= smallObject {
		// Small: read it (at most 8 MiB) so the hash is known before the
		// request, and store it in the same PUT.
		buf := make([]byte, opts.Size)
		_, rerr := io.ReadFull(hr, buf)
		if rerr == nil {
			lim.finish() // an empty source has not been looked at yet
		}
		if err := lim.failure(); err != nil {
			return err
		}
		if rerr != nil {
			return fmt.Errorf("s3: %s: source ended before %d bytes: %w", p, opts.Size, io.ErrUnexpectedEOF)
		}
		po.UserMetadata[shaMetaKey] = hashValue(hr.h.Sum(nil), opts.Size)
		_, err := f.mc.PutObject(ctx, f.bucket, key, bytes.NewReader(buf), opts.Size, po)
		return mapErr(err)
	}

	// Large or unknown size: stream it, hashing on the way. The hash is only
	// known at the end, and user metadata can only be set at the start of an
	// upload, so it is added afterwards by copying the object onto itself.
	// The copy is server-side but its cost grows with the object.
	// lim never lets a source that fails or holds too much complete the
	// upload, so nothing is replaced then.
	ui, err := f.mc.PutObject(ctx, f.bucket, key, hr, opts.Size, po)
	if serr := lim.failure(); serr != nil {
		f.abortUpload(ctx, key)
		return serr
	}
	if err != nil {
		f.abortUpload(ctx, key)
		return mapErr(err)
	}
	md[shaMetaKey] = hashValue(hr.h.Sum(nil), hr.n)
	// Conditional on the ETag of the upload: it fails instead of touching an
	// object that someone else replaced in between.
	return f.serverCopy(ctx,
		minio.CopyDestOptions{Bucket: f.bucket, Object: key, UserMetadata: md, ReplaceMetadata: true},
		minio.CopySrcOptions{Bucket: f.bucket, Object: key, MatchETag: strings.Trim(ui.ETag, `"`)},
		hr.n)
}

// serverCopy copies the object src, of size bytes, to dst on the server. Above
// maxCopyObject dst has to replace the metadata, or it is lost.
//
// Up to maxCopyObject it is one CopyObject request. ComposeObject is not used
// there: it copies every non-empty object as a multipart upload (its one-request
// shortcut needs a source option that its own validation rejects), which loses
// the Content-Type.
func (f *fs) serverCopy(ctx context.Context, dst minio.CopyDestOptions, src minio.CopySrcOptions, size int64) error {
	if size <= maxCopyObject {
		ui, err := f.mc.CopyObject(ctx, dst, src)
		if err != nil {
			return mapErr(err)
		}
		// S3 can answer a copy with 200 and an error in the body, which the
		// library decodes as an empty result. Taking that for success would
		// make Move delete the only copy.
		if ui.ETag == "" {
			return fmt.Errorf("s3: the server reported success for a copy but returned no result: %w", errServer)
		}
		return nil
	}
	if _, err := f.mc.ComposeObject(ctx, dst, src); err != nil {
		f.abortUpload(ctx, dst.Object) // a multipart copy is never aborted by the library
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

// copyKey copies one object on the server, with its headers and user
// metadata. A folder marker has no content to copy and is created again.
func (f *fs) copyKey(ctx context.Context, src, dst string) error {
	if strings.HasSuffix(src, "/") {
		_, err := f.mc.PutObject(ctx, f.bucket, dst, bytes.NewReader(nil), 0, minio.PutObjectOptions{})
		return mapErr(err)
	}
	st, err := f.mc.StatObject(ctx, f.bucket, src, minio.StatObjectOptions{})
	if err != nil {
		return mapErr(err)
	}
	do := minio.CopyDestOptions{Bucket: f.bucket, Object: dst}
	so := minio.CopySrcOptions{Bucket: f.bucket, Object: src}
	// The server keeps the metadata of a one-request copy, except that MinIO
	// loses Expires; a multipart copy starts a new object. In both cases hand
	// over what the source has, on condition that it is still the same.
	if !st.Expires.IsZero() || st.Size > maxCopyObject {
		do.UserMetadata, do.ReplaceMetadata = storedMetadata(st, st.Size <= maxCopyObject), true
		so.MatchETag = st.ETag
	}
	return f.serverCopy(ctx, do, so, st.Size)
}
