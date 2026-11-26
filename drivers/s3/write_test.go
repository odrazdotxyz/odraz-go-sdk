package s3_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	s3 "github.com/odrazdotxyz/odraz-go-sdk/drivers/s3"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
	"github.com/odrazdotxyz/odraz-go-sdk/files/fstest"
)

// realS3 returns the connection of a real S3-compatible server (for example a
// throwaway MinIO) or skips the test. Folder markers (zero-byte keys ending
// in "/") and streaming memory use cannot be tested against the in-process
// fake, which trims trailing slashes and rejects zero-byte PUTs, so those
// tests need this. Set ODRAZ_TEST_S3_ENDPOINT, ODRAZ_TEST_S3_BUCKET,
// ODRAZ_TEST_S3_KEY and ODRAZ_TEST_S3_SECRET, optionally
// ODRAZ_TEST_S3_TLS_INSECURE=1.
func realS3(t *testing.T) (endpoint, bucket, key, secret string) {
	t.Helper()
	endpoint = os.Getenv("ODRAZ_TEST_S3_ENDPOINT")
	bucket = os.Getenv("ODRAZ_TEST_S3_BUCKET")
	key = os.Getenv("ODRAZ_TEST_S3_KEY")
	secret = os.Getenv("ODRAZ_TEST_S3_SECRET")
	if endpoint == "" || bucket == "" || key == "" || secret == "" {
		t.Skip("set ODRAZ_TEST_S3_ENDPOINT, _BUCKET, _KEY and _SECRET to run against a real S3 server")
	}
	return
}

// openReal opens a drive on the real server, confined to a fresh random
// prefix that is removed afterwards. It returns the drive, a direct client and
// the bucket.
func openReal(t *testing.T) (files.FS, *minio.Client, string) {
	t.Helper()
	ep, bucket, key, secret := realS3(t)
	prefix := fmt.Sprintf("odraz-test-%d", rand.Int63())
	cfg := map[string]string{"endpoint": ep, "bucket": bucket, "prefix": prefix}
	if os.Getenv("ODRAZ_TEST_S3_TLS_INSECURE") == "1" {
		cfg[accounts.ConfigInsecure] = "true"
	}
	acc, store := account(t, key, secret, cfg)
	ctx := context.Background()
	h, err := accounts.Open(ctx, acc, store)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := files.ForAccount(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	fsys, err := cl.OpenDrive(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(strings.TrimPrefix(ep, "https://"), "http://")
	mc, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(key, secret, ""),
		Secure:       strings.HasPrefix(ep, "https://"),
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Delete(context.Background(), "/", true) })
	return fsys, mc, bucket
}

func writeBytes(t *testing.T, fsys files.FS, p string, data []byte, o files.WriteOptions) error {
	t.Helper()
	o.Size = int64(len(data))
	if o.SHA256 == "" {
		o.SHA256 = hexSHA(data) // so large uploads record it too
	}
	return fsys.Write(context.Background(), p, bytes.NewReader(data), o)
}

func readAll(t *testing.T, fsys files.FS, p string) []byte {
	t.Helper()
	r, err := fsys.Read(context.Background(), p, 0, -1)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hexSHA(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestWriteThenReadBack(t *testing.T) {
	fsys, _ := openFS(t, "")
	if err := writeBytes(t, fsys, "/hello.txt", []byte("hello world"), files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, fsys, "/hello.txt"); string(got) != "hello world" {
		t.Fatalf("got %q", got)
	}
	e, err := fsys.Stat(context.Background(), "/hello.txt")
	if err != nil || e.Size != 11 {
		t.Fatalf("stat %+v %v", e, err)
	}
}

func TestWriteMissingParentIsNotFound(t *testing.T) {
	fsys, _ := openFS(t, "")
	err := writeBytes(t, fsys, "/nope/x.txt", []byte("x"), files.WriteOptions{})
	if !errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteIntoImplicitFolder(t *testing.T) {
	fsys, mc := openFS(t, "")
	put(t, mc, "dir/seed", []byte("s"))
	if err := writeBytes(t, fsys, "/dir/x.txt", []byte("x"), files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, fsys, "/dir/x.txt"); string(got) != "x" {
		t.Fatalf("got %q", got)
	}
}

func TestWriteNoOverwrite(t *testing.T) {
	fsys, _ := openFS(t, "")
	if err := writeBytes(t, fsys, "/a.txt", []byte("one"), files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	err := writeBytes(t, fsys, "/a.txt", []byte("two"), files.WriteOptions{})
	if !errors.Is(err, odraz.ErrExists) {
		t.Fatalf("err = %v", err)
	}
	if got := readAll(t, fsys, "/a.txt"); string(got) != "one" {
		t.Fatalf("content changed to %q", got)
	}
}

func TestWriteOverwrite(t *testing.T) {
	fsys, _ := openFS(t, "")
	_ = writeBytes(t, fsys, "/a.txt", []byte("one"), files.WriteOptions{})
	if err := writeBytes(t, fsys, "/a.txt", []byte("three"), files.WriteOptions{Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, fsys, "/a.txt"); string(got) != "three" {
		t.Fatalf("got %q", got)
	}
}

func TestWriteStoresMetadata(t *testing.T) {
	fsys, mc := openFS(t, "")
	md := map[string]string{"Author": "Ana", "x-Note": "n1"}
	if err := writeBytes(t, fsys, "/m.txt", []byte("data"), files.WriteOptions{Metadata: md}); err != nil {
		t.Fatal(err)
	}
	st, err := mc.StatObject(context.Background(), fakeBucket, "m.txt", minio.StatObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for k, v := range st.UserMetadata {
		got[strings.ToLower(k)] = v
	}
	if got["author"] != "Ana" || got["x-note"] != "n1" {
		t.Fatalf("metadata = %v", st.UserMetadata)
	}
}

func TestWriteProgress(t *testing.T) {
	fsys, _ := openFS(t, "")
	data := bytes.Repeat([]byte("p"), 300000)
	var last, total int64
	calls := 0
	err := writeBytes(t, fsys, "/p.bin", data, files.WriteOptions{Progress: func(done, tot int64) {
		if done < last {
			t.Errorf("progress went back: %d after %d", done, last)
		}
		last, total = done, tot
		calls++
	}})
	if err != nil {
		t.Fatal(err)
	}
	if last != int64(len(data)) || total != int64(len(data)) || calls == 0 {
		t.Fatalf("last=%d total=%d calls=%d", last, total, calls)
	}
}

// noLen hides every method of the reader but Read, so nothing can learn its
// length. It makes the bytes of a deterministic stream and keeps their hash.
type noLen struct {
	left int64
	n    int64
	h    interface{ Write([]byte) (int, error) }
	peak *uint64
	base uint64
}

func (r *noLen) Read(b []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(b)) > r.left {
		b = b[:r.left]
	}
	for i := range b {
		b[i] = byte(r.n + int64(i)*7)
	}
	r.h.Write(b)
	r.n += int64(len(b))
	r.left -= int64(len(b))
	if r.peak != nil && r.n%(1<<20) < int64(len(b)) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if ms.HeapAlloc > r.base && ms.HeapAlloc-r.base > *r.peak {
			*r.peak = ms.HeapAlloc - r.base
		}
	}
	return len(b), nil
}

// The fake keeps the object in this process's heap, so the assertion is only
// honest against a real server.
func TestWriteUnknownSizeStreams(t *testing.T) {
	fsys, mc, bucket := openReal(t)
	const size = 24 << 20
	h := sha256.New()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	var peak uint64
	src := &noLen{left: size, h: h, peak: &peak, base: ms.HeapAlloc}
	if err := fsys.Write(context.Background(), "/big.bin", src, files.WriteOptions{Size: -1}); err != nil {
		t.Fatal(err)
	}
	if peak >= 16<<20 {
		t.Fatalf("heap grew by %d bytes while streaming 24 MB; the upload is buffered whole", peak)
	}
	t.Logf("peak heap growth %d bytes", peak)
	_ = mc
	_ = bucket
	r, err := fsys.Read(context.Background(), "/big.bin", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	h2 := sha256.New()
	n, err := io.Copy(h2, r)
	if err != nil || n != size {
		t.Fatalf("read back %d bytes, %v", n, err)
	}
	if !bytes.Equal(h.Sum(nil), h2.Sum(nil)) {
		t.Fatal("bytes differ")
	}
	got, err := files.SHA256(context.Background(), fsys, "/big.bin")
	if err != nil || got != hex.EncodeToString(h.Sum(nil)) {
		t.Fatalf("sha256 = %q, %v", got, err)
	}
}

func TestMkdirCreatesParentsIdempotent(t *testing.T) {
	fsys, _, _ := openReal(t)
	ctx := context.Background()
	if err := fsys.Mkdir(ctx, "/a/b/c"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/a", "/a/b", "/a/b/c"} {
		e, err := fsys.Stat(ctx, p)
		if err != nil || !e.IsDir {
			t.Fatalf("stat %s = %+v, %v", p, e, err)
		}
	}
	if err := fsys.Mkdir(ctx, "/a/b/c"); err != nil {
		t.Fatalf("second mkdir: %v", err)
	}
	if err := fsys.Mkdir(ctx, "/a/b"); err != nil {
		t.Fatalf("mkdir existing parent: %v", err)
	}
	// Write needs the parent to exist, and an empty folder counts.
	if err := writeBytes(t, fsys, "/a/b/c/f.txt", []byte("x"), files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := writeBytes(t, fsys, "/a/zzz/f.txt", []byte("x"), files.WriteOptions{}); !errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("write into missing parent: %v", err)
	}
}

func TestMkdirFileInTheWay(t *testing.T) {
	fsys, _, _ := openReal(t)
	ctx := context.Background()
	if err := writeBytes(t, fsys, "/f", []byte("x"), files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Mkdir(ctx, "/f"); !errors.Is(err, odraz.ErrExists) {
		t.Fatalf("mkdir over file: %v", err)
	}
	if err := fsys.Mkdir(ctx, "/f/sub"); !errors.Is(err, odraz.ErrExists) {
		t.Fatalf("mkdir below file: %v", err)
	}
}

func TestDeleteFile(t *testing.T) {
	fsys, _ := openFS(t, "")
	ctx := context.Background()
	_ = writeBytes(t, fsys, "/a.txt", []byte("x"), files.WriteOptions{})
	if err := fsys.Delete(ctx, "/a.txt", false); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Stat(ctx, "/a.txt"); !errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("stat after delete: %v", err)
	}
	if err := fsys.Delete(ctx, "/a.txt", false); !errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestDeleteNonRecursiveNotEmpty(t *testing.T) {
	fsys, mc := openFS(t, "")
	ctx := context.Background()
	put(t, mc, "d/x", []byte("x"))
	if err := fsys.Delete(ctx, "/d", false); !errors.Is(err, odraz.ErrNotEmpty) {
		t.Fatalf("err = %v", err)
	}
	if _, err := fsys.Stat(ctx, "/d/x"); err != nil {
		t.Fatalf("child gone: %v", err)
	}
	if err := fsys.Delete(ctx, "/d", true); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Stat(ctx, "/d"); !errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("stat after delete: %v", err)
	}
}

func TestDeleteEmptyFolderWithMarker(t *testing.T) {
	fsys, _, _ := openReal(t)
	ctx := context.Background()
	if err := fsys.Mkdir(ctx, "/e"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Delete(ctx, "/e", false); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Stat(ctx, "/e"); !errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("stat after delete: %v", err)
	}
}

func TestDeleteRecursiveBeyondOneBatch(t *testing.T) {
	fsys, mc := openFS(t, "")
	ctx := context.Background()
	const n = 2500
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			_, err := mc.PutObject(ctx, fakeBucket, fmt.Sprintf("many/f%04d", i), bytes.NewReader([]byte("x")), 1, minio.PutObjectOptions{})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if err := fsys.Delete(ctx, "/many", true); err != nil {
		t.Fatal(err)
	}
	left := 0
	for o := range mc.ListObjects(ctx, fakeBucket, minio.ListObjectsOptions{Prefix: "many/", Recursive: true}) {
		if o.Err != nil {
			t.Fatal(o.Err)
		}
		left++
	}
	if left != 0 {
		t.Fatalf("%d objects left", left)
	}
}

// Copies are PUTs without a body, which the in-process fake rejects, so
// everything that copies runs against a real server.
func TestMoveFileAndFolder(t *testing.T) {
	fsys, _, _ := openReal(t)
	ctx := context.Background()
	for _, d := range []string{"/src/sub", "/dst"} {
		if err := fsys.Mkdir(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	_ = writeBytes(t, fsys, "/src/one.txt", []byte("1"), files.WriteOptions{})
	_ = writeBytes(t, fsys, "/src/sub/two.txt", []byte("22"), files.WriteOptions{})
	_ = writeBytes(t, fsys, "/src/empty.txt", nil, files.WriteOptions{})
	if err := fsys.Move(ctx, "/src/one.txt", "/dst/uno.txt", false); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, fsys, "/dst/uno.txt"); string(got) != "1" {
		t.Fatalf("got %q", got)
	}
	if _, err := fsys.Stat(ctx, "/src/one.txt"); !errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("source remains: %v", err)
	}
	if err := fsys.Move(ctx, "/src", "/dst/moved", false); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, fsys, "/dst/moved/sub/two.txt"); string(got) != "22" {
		t.Fatalf("got %q", got)
	}
	if e, err := fsys.Stat(ctx, "/dst/moved/empty.txt"); err != nil || e.Size != 0 {
		t.Fatalf("empty file: %+v %v", e, err)
	}
	if _, err := fsys.Stat(ctx, "/src"); !errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("source folder remains: %v", err)
	}
	if err := fsys.Move(ctx, "/dst/moved", "/dst/moved/inside", false); !errors.Is(err, odraz.ErrInvalidPath) {
		t.Fatalf("move into itself: %v", err)
	}
}

func TestMoveMissing(t *testing.T) {
	fsys, _ := openFS(t, "")
	ctx := context.Background()
	_ = writeBytes(t, fsys, "/a", []byte("x"), files.WriteOptions{})
	if err := fsys.Move(ctx, "/nope", "/b", false); !errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("missing source: %v", err)
	}
	if err := fsys.Move(ctx, "/a", "/nodir/b", false); !errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("missing target parent: %v", err)
	}
}

func TestMoveOntoExisting(t *testing.T) {
	fsys, _, _ := openReal(t)
	ctx := context.Background()
	_ = writeBytes(t, fsys, "/a", []byte("A"), files.WriteOptions{})
	_ = writeBytes(t, fsys, "/b", []byte("B"), files.WriteOptions{})
	if err := fsys.Move(ctx, "/a", "/b", false); !errors.Is(err, odraz.ErrExists) {
		t.Fatalf("err = %v", err)
	}
	if got := readAll(t, fsys, "/b"); string(got) != "B" {
		t.Fatalf("target changed: %q", got)
	}
	if err := fsys.Move(ctx, "/a", "/b", true); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, fsys, "/b"); string(got) != "A" {
		t.Fatalf("got %q", got)
	}
}

func TestCopyServerSide(t *testing.T) {
	fsys, _, _ := openReal(t)
	ctx := context.Background()
	if err := fsys.Mkdir(ctx, "/d"); err != nil {
		t.Fatal(err)
	}
	if err := writeBytes(t, fsys, "/orig.txt", []byte("content"), files.WriteOptions{Metadata: map[string]string{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	cp, ok := fsys.(files.Copier)
	if !ok {
		t.Fatal("FS is not a files.Copier")
	}
	if err := files.Copy(ctx, fsys, "/orig.txt", "/d/copy.txt", false); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, fsys, "/d/copy.txt"); string(got) != "content" {
		t.Fatalf("got %q", got)
	}
	if got := readAll(t, fsys, "/orig.txt"); string(got) != "content" {
		t.Fatalf("source changed: %q", got)
	}
	if err := cp.Copy(ctx, "/orig.txt", "/d/copy.txt", false); !errors.Is(err, odraz.ErrExists) {
		t.Fatalf("copy onto existing: %v", err)
	}
	if err := cp.Copy(ctx, "/orig.txt", "/d/copy.txt", true); err != nil {
		t.Fatal(err)
	}
	// A folder copies with everything below it.
	_ = fsys.Mkdir(ctx, "/tree/a")
	_ = writeBytes(t, fsys, "/tree/a/b.txt", []byte("b"), files.WriteOptions{})
	if err := cp.Copy(ctx, "/tree", "/d/tree2", false); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, fsys, "/d/tree2/a/b.txt"); string(got) != "b" {
		t.Fatalf("got %q", got)
	}
	if got := readAll(t, fsys, "/tree/a/b.txt"); string(got) != "b" {
		t.Fatalf("source tree changed: %q", got)
	}
}

func TestMoveOntoExistingNoOverwriteFake(t *testing.T) {
	fsys, _ := openFS(t, "")
	_ = writeBytes(t, fsys, "/a", []byte("A"), files.WriteOptions{})
	_ = writeBytes(t, fsys, "/b", []byte("B"), files.WriteOptions{})
	if err := fsys.Move(context.Background(), "/a", "/b", false); !errors.Is(err, odraz.ErrExists) {
		t.Fatalf("err = %v", err)
	}
}

func TestSHA256StoredOnWrite(t *testing.T) {
	fsys, _ := openFS(t, "")
	data := []byte("the quick brown fox")
	if err := writeBytes(t, fsys, "/f.txt", data, files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	h, ok := fsys.(files.Hasher)
	if !ok {
		t.Fatal("FS is not a files.Hasher")
	}
	got, err := h.SHA256(context.Background(), "/f.txt")
	if err != nil || got != hexSHA(data) {
		t.Fatalf("sha256 = %q, %v; want %s", got, err, hexSHA(data))
	}
}

// Against a real server: a small object (one PUT) and a multipart one both
// report the hash of the whole object, and unknown size works too.
func TestSHA256SmallAndMultipartReal(t *testing.T) {
	fsys, _, _ := openReal(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		size int
		sz   int64 // Size option; -1 unknown
	}{{"small", 1000, 1000}, {"small-unknown", 1000, -1}, {"multipart", 20 << 20, 20 << 20}, {"multipart-unknown", 20 << 20, -1}} {
		data := make([]byte, tc.size)
		rand.New(rand.NewSource(int64(tc.size))).Read(data)
		p := "/" + tc.name
		o := files.WriteOptions{Size: tc.sz}
		if tc.sz >= 0 {
			o.SHA256 = hexSHA(data)
		}
		if err := fsys.Write(ctx, p, bytes.NewReader(data), o); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tc.sz < 0 { // the size is part of the stored value, so none is stored
			if _, err := fsys.(files.Hasher).SHA256(ctx, p); !errors.Is(err, odraz.ErrUnsupported) {
				t.Fatalf("%s: sha256 err = %v", tc.name, err)
			}
			continue
		}
		got, err := fsys.(files.Hasher).SHA256(ctx, p)
		if err != nil || got != hexSHA(data) {
			t.Fatalf("%s: sha256 = %q, %v; want %s", tc.name, got, err, hexSHA(data))
		}
		cp := p + "-copy"
		if err := fsys.(files.Copier).Copy(ctx, p, cp, false); err != nil {
			t.Fatalf("%s copy: %v", tc.name, err)
		}
		got, err = fsys.(files.Hasher).SHA256(ctx, cp)
		if err != nil || got != hexSHA(data) {
			t.Fatalf("%s: copy sha256 = %q, %v", tc.name, got, err)
		}
	}
}

func TestSHA256UnsupportedForForeignObject(t *testing.T) {
	fsys, mc := openFS(t, "")
	put(t, mc, "foreign.txt", []byte("not ours"))
	_, err := fsys.(files.Hasher).SHA256(context.Background(), "/foreign.txt")
	if !errors.Is(err, odraz.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
	// The helper then falls back to reading the object.
	got, err := files.SHA256(context.Background(), fsys, "/foreign.txt")
	if err != nil || got != hexSHA([]byte("not ours")) {
		t.Fatalf("helper = %q, %v", got, err)
	}
}

func TestListIsSorted(t *testing.T) {
	fsys, mc := openFS(t, "")
	for _, k := range []string{"s/zeta", "s/alpha", "s/mid/x", "s/beta", "s/omega", "s/Aaa"} {
		put(t, mc, k, []byte("x"))
	}
	got, err := fsys.List(context.Background(), "/s")
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, len(got))
	for i, e := range got {
		paths[i] = e.Path
	}
	if !sort.StringsAreSorted(paths) || len(paths) != 6 {
		t.Fatalf("paths = %v", paths)
	}
}

// The conformance suite, against a real server (needs folder markers). This is
// the only conformance coverage of the adapter: the in-process fake cannot
// store folder markers or zero-byte objects, and fstest.Run cannot be subset.
// Run it with testenv/minio (see the package doc).
func TestConformanceS3(t *testing.T) {
	realS3(t)
	fstest.Run(t, func(t *testing.T) files.FS {
		fsys, _, _ := openReal(t)
		return fsys
	})
}

// Concurrent writers of a new key without Overwrite: exactly one wins. The
// Stat before the put cannot see a writer that is still uploading, so this
// only holds where the server honours If-None-Match: *.
func TestWriteNoOverwriteRaceReal(t *testing.T) {
	fsys, _, _ := openReal(t)
	const n = 8
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			<-start
			errs <- writeBytes(t, fsys, "/race.txt", bytes.Repeat([]byte{byte('a' + i)}, 1<<20), files.WriteOptions{})
		}(i)
	}
	close(start)
	won := 0
	for i := 0; i < n; i++ {
		err := <-errs
		switch {
		case err == nil:
			won++
		case !errors.Is(err, odraz.ErrExists):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d writers succeeded, want 1", won)
	}
}

func absent(t *testing.T, fsys files.FS, p string) {
	t.Helper()
	if _, err := fsys.Stat(context.Background(), p); !errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("%s should not exist: %v", p, err)
	}
}

func TestWriteLongerThanDeclaredSmall(t *testing.T) {
	fsys, _ := openFS(t, "")
	err := fsys.Write(context.Background(), "/long.txt", bytes.NewReader([]byte("0123456789")), files.WriteOptions{Size: 5})
	if !errors.Is(err, odraz.ErrInvalidPath) {
		t.Fatalf("err = %v", err)
	}
	absent(t, fsys, "/long.txt")
}

func TestWriteLongerThanDeclaredLargeReal(t *testing.T) {
	fsys, _, _ := openReal(t)
	const size = 9 << 20
	data := make([]byte, size+10)
	err := fsys.Write(context.Background(), "/long.bin", bytes.NewReader(data), files.WriteOptions{Size: size})
	if !errors.Is(err, odraz.ErrInvalidPath) {
		t.Fatalf("err = %v", err)
	}
	absent(t, fsys, "/long.bin")
}

// A stream over the cap must fail and leave nothing, not be stored cut off.
func TestWriteUnknownSizeOverCapReal(t *testing.T) {
	fsys, _, _ := openReal(t)
	defer s3.SetUnknownSizeLimits(5<<20, 15<<20)()
	ctx := context.Background()
	err := fsys.Write(ctx, "/over.bin", bytes.NewReader(make([]byte, 20<<20)), files.WriteOptions{Size: -1})
	if !errors.Is(err, odraz.ErrUnsupported) || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("err = %v", err)
	}
	absent(t, fsys, "/over.bin")
	// Exactly at the cap is fine.
	if err := fsys.Write(ctx, "/at.bin", bytes.NewReader(make([]byte, 15<<20)), files.WriteOptions{Size: -1}); err != nil {
		t.Fatal(err)
	}
	if e, err := fsys.Stat(ctx, "/at.bin"); err != nil || e.Size != 15<<20 {
		t.Fatalf("stat %+v %v", e, err)
	}
}

// Every way of spelling the reserved key, including doubled and tripled
// prefixes that minio-go would strip down to it, and other x-amz- keys.
var reservedSpellings = []string{
	"x-amz-meta-odraz-sha256", "X-Amz-Meta-Odraz-Sha256", "Odraz-Sha256", "ODRAZ-SHA256",
	"x-amz-meta-x-amz-meta-odraz-sha256", "X-AMZ-Meta-x-Amz-META-Odraz-Sha256",
	"x-amz-meta-X-Amz-Meta-x-amz-meta-odraz-sha256", "x-amz-odraz-sha256",
}

func TestReservedMetadataKeyCannotBeSet(t *testing.T) {
	fsys, _ := openFS(t, "")
	fake := strings.Repeat("0", 64) + ":4"
	for i, key := range reservedSpellings {
		p := fmt.Sprintf("/r%d.txt", i)
		if err := writeBytes(t, fsys, p, []byte("data"), files.WriteOptions{Metadata: map[string]string{key: fake, "keep": "me"}}); err != nil {
			t.Fatal(err)
		}
		got, err := fsys.(files.Hasher).SHA256(context.Background(), p)
		if err != nil || got != hexSHA([]byte("data")) {
			t.Fatalf("%s: sha256 = %q, %v", key, got, err)
		}
	}
	// All spellings in one write, repeated so map order cannot hide a clash.
	all := map[string]string{"keep": "me"}
	for _, k := range reservedSpellings {
		all[k] = fake
	}
	for i := 0; i < 20; i++ {
		if err := writeBytes(t, fsys, "/all.txt", []byte("data"), files.WriteOptions{Overwrite: true, Metadata: all}); err != nil {
			t.Fatal(err)
		}
		got, err := fsys.(files.Hasher).SHA256(context.Background(), "/all.txt")
		if err != nil || got != hexSHA([]byte("data")) {
			t.Fatalf("all spellings: sha256 = %q, %v", got, err)
		}
	}
}

func TestReservedMetadataKeyLargeReal(t *testing.T) {
	fsys, _, _ := openReal(t)
	data := make([]byte, 9<<20)
	rand.New(rand.NewSource(1)).Read(data)
	for i := 0; i < 5; i++ {
		for _, tc := range []struct {
			size int64
			sum  string
		}{{int64(len(data)), ""}, {int64(len(data)), hexSHA(data)}, {-1, ""}} {
			md := map[string]string{"keep": "me"}
			for _, k := range reservedSpellings {
				md[k] = strings.Repeat("0", 64) + ":9437184"
			}
			if err := fsys.Write(context.Background(), "/r.bin", bytes.NewReader(data), files.WriteOptions{Size: tc.size, SHA256: tc.sum, Overwrite: true, Metadata: md}); err != nil {
				t.Fatal(err)
			}
			got, err := fsys.(files.Hasher).SHA256(context.Background(), "/r.bin")
			if tc.sum == "" && !errors.Is(err, odraz.ErrUnsupported) {
				t.Fatalf("size %d: caller metadata set the hash: %q, %v", tc.size, got, err)
			}
			if tc.sum != "" && (err != nil || got != tc.sum) {
				t.Fatalf("size %d: sha256 = %q, %v", tc.size, got, err)
			}
		}
	}
}

// Content rewritten by another tool that keeps the user metadata must not
// keep vouching for the old hash.
func TestSHA256RejectsRewrittenAndMalformed(t *testing.T) {
	fsys, mc := openFS(t, "")
	ctx := context.Background()
	if err := writeBytes(t, fsys, "/h.txt", []byte("original"), files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err := mc.StatObject(ctx, fakeBucket, "h.txt", minio.StatObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stored := ""
	for k, v := range st.UserMetadata {
		if strings.EqualFold(k, "odraz-sha256") {
			stored = v
		}
	}
	if !strings.HasPrefix(stored, hexSHA([]byte("original"))+":8") {
		t.Fatalf("stored value = %q", stored)
	}
	rewrite := func(key, value string, data []byte) {
		t.Helper()
		_, err := mc.PutObject(ctx, fakeBucket, key, bytes.NewReader(data), int64(len(data)),
			minio.PutObjectOptions{UserMetadata: map[string]string{"Odraz-Sha256": value}})
		if err != nil {
			t.Fatal(err)
		}
	}
	hs := fsys.(files.Hasher)
	rewrite("h.txt", stored, []byte("a different length"))
	if _, err := hs.SHA256(ctx, "/h.txt"); !errors.Is(err, odraz.ErrUnsupported) {
		t.Fatalf("rewritten: %v", err)
	}
	good := hexSHA([]byte("12345"))
	for i, bad := range []string{good, good + ":", good + ":x", good + ":-5", strings.ToUpper(good) + ":5", good[:63] + ":5", "zz" + good[2:] + ":5", good + ":5:5"} {
		key := fmt.Sprintf("bad%d.txt", i)
		rewrite(key, bad, []byte("12345"))
		if _, err := hs.SHA256(ctx, "/"+key); !errors.Is(err, odraz.ErrUnsupported) {
			t.Fatalf("%q accepted: %v", bad, err)
		}
	}
	rewrite("ok.txt", good+":5", []byte("12345"))
	if got, err := hs.SHA256(ctx, "/ok.txt"); err != nil || got != good {
		t.Fatalf("well formed: %q %v", got, err)
	}
}

// A cancelled upload must not leave its parts behind. (MinIO filters the
// listing of incomplete uploads by prefix unreliably, so list everything and
// match the key's name, which is unique to this run.)
func TestCancelledUploadLeavesNoIncompleteUploadReal(t *testing.T) {
	fsys, mc, bucket := openReal(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	name := fmt.Sprintf("c-%d.bin", rand.Int63())
	src := &cancelAfter{left: 40 << 20, after: 12 << 20, cancel: cancel}
	if err := fsys.Write(ctx, "/"+name, src, files.WriteOptions{Size: -1}); err == nil {
		t.Fatal("write succeeded")
	}
	n := 0
	for u := range mc.ListIncompleteUploads(context.Background(), bucket, "", true) {
		if u.Err != nil {
			t.Fatal(u.Err)
		}
		if strings.HasSuffix(u.Key, "/"+name) {
			n++
			t.Logf("left over: %s", u.Key)
		}
	}
	if n != 0 {
		t.Fatalf("%d incomplete uploads left", n)
	}
	absent(t, fsys, "/"+name)
}

type cancelAfter struct {
	left, after, n int64
	cancel         func()
}

func (c *cancelAfter) Read(b []byte) (int, error) {
	if c.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(b)) > c.left {
		b = b[:c.left]
	}
	c.left -= int64(len(b))
	c.n += int64(len(b))
	if c.n >= c.after {
		c.cancel()
	}
	return len(b), nil
}

// patterned returns n bytes that differ from one seed to the next.
func patterned(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// keeps asserts that p still holds old, with the hash recorded for it.
func keeps(t *testing.T, fsys files.FS, p string, old []byte) {
	t.Helper()
	if got := readAll(t, fsys, p); !bytes.Equal(got, old) {
		t.Fatalf("%s: previous object changed: %d bytes", p, len(got))
	}
	if got, err := fsys.(files.Hasher).SHA256(context.Background(), p); err != nil || got != hexSHA(old) {
		t.Fatalf("%s: previous hash = %q, %v", p, got, err)
	}
}

// A source longer than declared must not touch the object it was to replace.
func TestOverLongOverwriteKeepsPreviousReal(t *testing.T) {
	fsys, _, _ := openReal(t)
	ctx := context.Background()
	old := []byte("the only copy of the original")
	for _, size := range []int{0, 5, 1 << 20, 8 << 20, 9 << 20, 16 << 20, 20 << 20} {
		for _, extra := range []int{1, 10} {
			p := fmt.Sprintf("/o-%d-%d.bin", size, extra)
			if err := writeBytes(t, fsys, p, old, files.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			src := bytes.NewReader(patterned(size+extra, 1))
			err := fsys.Write(ctx, p, src, files.WriteOptions{Size: int64(size), Overwrite: true})
			if !errors.Is(err, odraz.ErrInvalidPath) || !strings.Contains(err.Error(), "longer than the declared") {
				t.Fatalf("size %d+%d: err = %v", size, extra, err)
			}
			keeps(t, fsys, p, old)
		}
		// The exact length succeeds.
		p := fmt.Sprintf("/exact-%d.bin", size)
		data := patterned(size, 2)
		if err := writeBytes(t, fsys, p, data, files.WriteOptions{}); err != nil {
			t.Fatalf("exact %d: %v", size, err)
		}
		keeps(t, fsys, p, data)
	}
}

func TestOverCapOverwriteKeepsPreviousReal(t *testing.T) {
	fsys, _, _ := openReal(t)
	defer s3.SetUnknownSizeLimits(5<<20, 15<<20)()
	ctx := context.Background()
	old := []byte("the only copy of the original")
	if err := writeBytes(t, fsys, "/cap.bin", old, files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, extra := range []int{1, 5 << 20} {
		err := fsys.Write(ctx, "/cap.bin", bytes.NewReader(patterned(15<<20+extra, 3)), files.WriteOptions{Size: -1, Overwrite: true})
		if !errors.Is(err, odraz.ErrUnsupported) || !strings.Contains(err.Error(), "maximum") {
			t.Fatalf("+%d: err = %v", extra, err)
		}
		keeps(t, fsys, "/cap.bin", old)
	}
	for _, size := range []int{15<<20 - 1, 15 << 20} {
		data := patterned(size, 4)
		if err := fsys.Write(ctx, "/cap.bin", bytes.NewReader(data), files.WriteOptions{Size: -1, Overwrite: true}); err != nil {
			t.Fatalf("%d: %v", size, err)
		}
		if got := readAll(t, fsys, "/cap.bin"); !bytes.Equal(got, data) {
			t.Fatalf("%d: stored %d bytes", size, len(got))
		}
	}
}

// failAfter yields n bytes and then fails with err.
type failAfter struct {
	n   int
	err error
}

func (r *failAfter) Read(b []byte) (int, error) {
	if r.n == 0 {
		return 0, r.err
	}
	if len(b) > r.n {
		b = b[:r.n]
	}
	r.n -= len(b)
	return len(b), nil
}

// A source that fails, also with io.ErrUnexpectedEOF (what a truncated HTTP
// body gives), or ends early must never be stored cut off.
func TestFailedSourceKeepsPreviousReal(t *testing.T) {
	fsys, _, _ := openReal(t)
	defer s3.SetUnknownSizeLimits(5<<20, 15<<20)()
	ctx := context.Background()
	old := []byte("the only copy of the original")
	if err := writeBytes(t, fsys, "/f.bin", old, files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	for _, srcErr := range []error{boom, io.ErrUnexpectedEOF} {
		for _, n := range []int{100, 5 << 20, 7 << 20} {
			for _, size := range []int64{-1, int64(n) + 100, 17 << 20} {
				err := fsys.Write(ctx, "/f.bin", &failAfter{n, srcErr}, files.WriteOptions{Size: size, Overwrite: true})
				if !errors.Is(err, srcErr) {
					t.Fatalf("%v after %d, size %d: err = %v", srcErr, n, size, err)
				}
				keeps(t, fsys, "/f.bin", old)
			}
		}
	}
	for _, size := range []int64{100, 9 << 20, 17 << 20} {
		if err := fsys.Write(ctx, "/f.bin", bytes.NewReader(make([]byte, size-1)), files.WriteOptions{Size: size, Overwrite: true}); err == nil {
			t.Fatalf("short source, size %d: no error", size)
		}
		keeps(t, fsys, "/f.bin", old)
	}
}

// recorder stands in front of the fake. It keeps the headers of every request,
// makes the fake look like a server that honours If-None-Match on the probe,
// and answers server-side copies (which the fake rejects) with copyBody.
type recorder struct {
	mu         sync.Mutex
	reqs       []*http.Request
	copyBody   string
	failDelete bool
}

const copyOK = `<CopyObjectResult><ETag>"d41d8cd98f00b204e9800998ecf8427e"</ETag><LastModified>2026-01-02T03:04:05.000Z</LastModified></CopyObjectResult>`

func newRecorder(t *testing.T) (*recorder, string) {
	t.Helper()
	rec := &recorder{copyBody: copyOK}
	next := fakeHandler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, r.Clone(context.Background()))
		body, failDelete := rec.copyBody, rec.failDelete
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "":
			io.WriteString(w, body)
		case r.Method == http.MethodPut && r.Header.Get("If-None-Match") == "*" && strings.Contains(r.URL.Path, "probe-"):
			w.WriteHeader(http.StatusPreconditionFailed)
			io.WriteString(w, `<Error><Code>PreconditionFailed</Code></Error>`)
		case r.Method == http.MethodDelete && failDelete:
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
		default:
			w.Header().Del("Content-Type")
			next.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return rec, srv.URL
}

func (rec *recorder) matching(f func(*http.Request) bool) (out []*http.Request) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, r := range rec.reqs {
		if f(r) {
			out = append(out, r)
		}
	}
	return out
}

func TestConditionalHeaderOnlyOnSinglePut(t *testing.T) {
	rec, ep := newRecorder(t)
	fsys, _ := openFSAt(t, ep, "")
	for _, tc := range []struct {
		name      string
		size      int
		unknown   bool
		multipart bool
	}{
		{"small", 100, false, false},
		{"streamed-single", 9 << 20, false, false},
		{"largest-single", 16 << 20, false, false},
		{"multipart", 16<<20 + 1, false, true},
		{"unknown", 100, true, true},
	} {
		size := int64(tc.size)
		if tc.unknown {
			size = -1
		}
		if err := fsys.Write(context.Background(), "/"+tc.name, bytes.NewReader(make([]byte, tc.size)), files.WriteOptions{Size: size}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		reqs := rec.matching(func(r *http.Request) bool {
			return r.URL.Path == "/"+fakeBucket+"/"+tc.name && r.Header.Get("X-Amz-Copy-Source") == "" &&
				(r.Method == http.MethodPut || r.Method == http.MethodPost)
		})
		multipart := false
		conditional := 0
		for _, r := range reqs {
			multipart = multipart || r.URL.Query().Has("uploads")
			if r.Header.Get("If-None-Match") != "" {
				conditional++
			}
		}
		want := 1
		if tc.multipart {
			want = 0
		}
		if len(reqs) == 0 || multipart != tc.multipart || conditional != want {
			t.Fatalf("%s: %d upload requests, multipart %v, %d conditional (want %d)", tc.name, len(reqs), multipart, conditional, want)
		}
	}
}

// CopyObject can answer 200 with an error in the body (AWS documents this).
// That must not count as a copy: Move would delete the only object.
func TestCopyErrorInSuccessResponseKeepsSource(t *testing.T) {
	rec, ep := newRecorder(t)
	fsys, mc := openFSAt(t, ep, "")
	put(t, mc, "src.txt", []byte("precious"))
	rec.mu.Lock()
	rec.copyBody = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>InternalError</Code><Message>try again</Message></Error>`
	rec.mu.Unlock()
	if err := fsys.Move(context.Background(), "/src.txt", "/dst.txt", false); err == nil {
		t.Error("move reported success")
	}
	keepsBytes := readAll(t, fsys, "/src.txt")
	if string(keepsBytes) != "precious" {
		t.Fatalf("source = %q", keepsBytes)
	}
}

var stdHeaders = map[string]string{
	"content-TYPE":        "image/x-custom",
	"Cache-Control":       "max-age=60",
	"content-disposition": `attachment; filename="a.bin"`,
	"Content-Encoding":    "identity",
	"CONTENT-LANGUAGE":    "de",
	"Expires":             "Wed, 21 Oct 2037 07:28:00 GMT",
}

// checkHeaders asserts the stored headers, user metadata and hash of key.
func checkHeaders(t *testing.T, fsys files.FS, mc *minio.Client, bucket, prefix, p string, data []byte, hashed bool) {
	t.Helper()
	ctx := context.Background()
	e, err := fsys.Stat(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	key := prefix + p
	st, err := mc.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err != nil {
		t.Fatalf("%s: stat %q: %v", p, key, err)
	}
	for k, want := range stdHeaders {
		got := st.Metadata.Get(k)
		if http.CanonicalHeaderKey(k) == "Expires" {
			if !st.Expires.IsZero() {
				got = st.Expires.UTC().Format(http.TimeFormat)
			}
		}
		if got != want {
			t.Errorf("%s: %s = %q, want %q", p, k, got, want)
		}
	}
	if e.MimeType != "image/x-custom" {
		t.Errorf("%s: MimeType = %q", p, e.MimeType)
	}
	if st.UserMetadata["Source"] != "camera" {
		t.Errorf("%s: user metadata = %v", p, st.UserMetadata)
	}
	got, err := fsys.(files.Hasher).SHA256(ctx, p)
	if hashed && (err != nil || got != hexSHA(data)) {
		t.Errorf("%s: sha256 = %q, %v", p, got, err)
	}
	if !hashed && !errors.Is(err, odraz.ErrUnsupported) {
		t.Errorf("%s: unexpected stored hash %q, %v", p, got, err)
	}
}

// drivePrefix finds the random key prefix of a drive of openReal by writing an
// object and looking for it.
func drivePrefix(t *testing.T, fsys files.FS, mc *minio.Client, bucket string) string {
	t.Helper()
	name := fmt.Sprintf("/where-am-i-%d", rand.Int63())
	if err := writeBytes(t, fsys, name, []byte("x"), files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for o := range mc.ListObjects(context.Background(), bucket, minio.ListObjectsOptions{Recursive: true}) {
		if o.Err != nil {
			t.Fatal(o.Err)
		}
		if p, ok := strings.CutSuffix(o.Key, name); ok {
			return p
		}
	}
	t.Fatal("drive prefix not found")
	return ""
}

func headerMeta() map[string]string {
	m := map[string]string{"Source": "camera"}
	for k, v := range stdHeaders {
		m[k] = v
	}
	return m
}

func TestStandardHeaderMetadataReal(t *testing.T) {
	fsys, mc, bucket := openReal(t)
	ctx := context.Background()
	prefix := drivePrefix(t, fsys, mc, bucket)
	for _, tc := range []struct {
		name    string
		size    int
		unknown bool
	}{{"h-small", 100, false}, {"h-single", 9 << 20, false}, {"h-multi", 20 << 20, false}, {"h-unknown", 9 << 20, true}} {
		data := patterned(tc.size, 5)
		size := int64(tc.size)
		if tc.unknown {
			size = -1
		}
		p := "/" + tc.name
		o := files.WriteOptions{Size: size, Metadata: headerMeta()}
		if !tc.unknown {
			o.SHA256 = hexSHA(data)
		}
		if err := fsys.Write(ctx, p, bytes.NewReader(data), o); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		checkHeaders(t, fsys, mc, bucket, prefix, p, data, !tc.unknown)
		if err := fsys.(files.Copier).Copy(ctx, p, p+"-copy", false); err != nil {
			t.Fatalf("%s copy: %v", tc.name, err)
		}
		checkHeaders(t, fsys, mc, bucket, prefix, p+"-copy", data, !tc.unknown)
		if err := fsys.Move(ctx, p+"-copy", p+"-moved", false); err != nil {
			t.Fatalf("%s move: %v", tc.name, err)
		}
		checkHeaders(t, fsys, mc, bucket, prefix, p+"-moved", data, !tc.unknown)
	}
}

// Content-Type from the file name, with no metadata from the caller, survives
// a large upload, Copy, Move and a folder copy.
func TestCopyKeepsContentTypeReal(t *testing.T) {
	fsys, _, _ := openReal(t)
	ctx := context.Background()
	if err := fsys.Mkdir(ctx, "/d"); err != nil {
		t.Fatal(err)
	}
	is := func(p string) {
		t.Helper()
		if e, err := fsys.Stat(ctx, p); err != nil || e.MimeType != "image/png" {
			t.Errorf("%s: MimeType = %q, %v", p, e.MimeType, err)
		}
	}
	for _, size := range []int{10, 9 << 20, 20 << 20} {
		name := fmt.Sprintf("%d.png", size)
		data := patterned(size, 6)
		if err := writeBytes(t, fsys, "/d/"+name, data, files.WriteOptions{Metadata: map[string]string{"k": "v"}}); err != nil {
			t.Fatal(err)
		}
		is("/d/" + name)
		cp := "/c-" + name + ".bin"
		if err := fsys.(files.Copier).Copy(ctx, "/d/"+name, cp, false); err != nil {
			t.Fatal(err)
		}
		is(cp)
		if err := fsys.Move(ctx, cp, cp+".moved", false); err != nil {
			t.Fatal(err)
		}
		is(cp + ".moved")
		keeps(t, fsys, cp+".moved", data)
	}
	if err := fsys.(files.Copier).Copy(ctx, "/d", "/d2", false); err != nil {
		t.Fatal(err)
	}
	is("/d2/10.png")
	is("/d2/20971520.png")
}

func TestMetadataWithControlCharactersIsInvalid(t *testing.T) {
	fsys, _ := openFS(t, "")
	for _, key := range []string{"note", "Cache-Control", "Content-Type", "Content-Disposition", "Content-Encoding", "Content-Language", "Expires"} {
		for _, v := range []string{"a\rb", "a\nb", "a\r\nX-Injected: 1", "a\x00b"} {
			err := writeBytes(t, fsys, "/m", []byte("x"), files.WriteOptions{Metadata: map[string]string{key: v}})
			if !errors.Is(err, odraz.ErrInvalidPath) || !strings.Contains(err.Error(), "metadata") || strings.Contains(err.Error(), "server error") {
				t.Fatalf("%s %q: err = %v", key, v, err)
			}
			absent(t, fsys, "/m")
		}
	}
	for _, md := range []map[string]string{{"bad name": "v"}, {"x-minio-thing": "v"}, {"Expires": "tomorrow"}} {
		err := writeBytes(t, fsys, "/m", []byte("x"), files.WriteOptions{Metadata: md})
		if !errors.Is(err, odraz.ErrInvalidPath) || !strings.Contains(err.Error(), "metadata") {
			t.Fatalf("%v: err = %v", md, err)
		}
		absent(t, fsys, "/m")
	}
}

func TestProbeKeyUnderOdrazFolder(t *testing.T) {
	for _, failDelete := range []bool{false, true} {
		rec, ep := newRecorder(t)
		rec.failDelete = failDelete
		fsys, _ := openFSAt(t, ep, "pre")
		if err := writeBytes(t, fsys, "/a", []byte("x"), files.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		probes := rec.matching(func(r *http.Request) bool { return strings.Contains(r.URL.Path, "probe-") })
		if len(probes) == 0 {
			t.Fatal("no probe")
		}
		for _, r := range probes {
			if !strings.HasPrefix(r.URL.Path, "/"+fakeBucket+"/pre/.odraz/probe-") {
				t.Fatalf("probe key %s", r.URL.Path)
			}
		}
		root, err := fsys.List(context.Background(), "/")
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range root {
			names = append(names, e.Name)
		}
		want := "a"
		if failDelete {
			want = ".odraz,a"
		}
		if got := strings.Join(names, ","); got != want {
			t.Fatalf("failDelete %v: root = %s, want %s", failDelete, got, want)
		}
	}
}

// A source of unknown size that fails once with io.ErrUnexpectedEOF and then
// reports io.EOF must not be taken for a complete stream.
func TestTruncatedSourceIsNotStored(t *testing.T) {
	fsys, _ := openFS(t, "")
	src := io.MultiReader(strings.NewReader("half"), &failOnce{})
	err := fsys.Write(context.Background(), "/t", src, files.WriteOptions{Size: -1})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v", err)
	}
	absent(t, fsys, "/t")
}

type failOnce struct{ done bool }

func (r *failOnce) Read([]byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return 0, io.ErrUnexpectedEOF
}

// Above maxCopyObject the copy is multipart: it has to carry the headers and
// metadata over itself.
func TestMultipartCopyKeepsHeadersReal(t *testing.T) {
	fsys, mc, bucket := openReal(t)
	defer s3.SetMaxCopyObject(1 << 20)()
	ctx := context.Background()
	prefix := drivePrefix(t, fsys, mc, bucket)
	data := patterned(9<<20, 7)
	if err := writeBytes(t, fsys, "/big", data, files.WriteOptions{Metadata: headerMeta()}); err != nil {
		t.Fatal(err)
	}
	checkHeaders(t, fsys, mc, bucket, prefix, "/big", data, true)
	if err := fsys.(files.Copier).Copy(ctx, "/big", "/big-copy", false); err != nil {
		t.Fatal(err)
	}
	checkHeaders(t, fsys, mc, bucket, prefix, "/big-copy", data, true)
	st, err := mc.StatObject(ctx, bucket, prefix+"/big-copy", minio.StatObjectOptions{})
	if err != nil || !strings.HasSuffix(st.ETag, "-1") {
		t.Fatalf("not a multipart copy: etag %q, %v", st.ETag, err)
	}
}

// A large upload is one upload: no second server-side copy, and a supplied hash
// goes out with it.
func TestLargeUploadMakesNoSecondCopy(t *testing.T) {
	rec, ep := newRecorder(t)
	fsys, _ := openFSAt(t, ep, "")
	if err := writeBytes(t, fsys, "/big", make([]byte, 9<<20), files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := len(rec.matching(func(r *http.Request) bool { return r.Header.Get("X-Amz-Copy-Source") != "" })); n != 0 {
		t.Fatalf("%d copy requests", n)
	}
	if n := len(rec.matching(func(r *http.Request) bool { return r.Header.Get("X-Amz-Meta-Odraz-Sha256") != "" })); n != 1 {
		t.Fatalf("%d requests carry the hash", n)
	}
}

// What the package doc says about reading at the end and about metadata that is
// not ASCII.
func TestDocumentedBehaviourReal(t *testing.T) {
	fsys, mc, bucket := openReal(t)
	ctx := context.Background()
	prefix := drivePrefix(t, fsys, mc, bucket)
	if err := writeBytes(t, fsys, "/empty", nil, files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := writeBytes(t, fsys, "/ten", []byte("0123456789"), files.WriteOptions{Metadata: map[string]string{"place": "Köln 日本"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		p        string
		off, n   int64
		fails    bool
		wantSize int
	}{
		{"/empty", 0, -1, false, 0}, {"/empty", 0, 0, false, 0}, {"/empty", 0, 1, true, 0}, {"/empty", 1, -1, true, 0},
		{"/ten", 10, -1, true, 0}, {"/ten", 11, -1, true, 0}, {"/ten", 10, 5, true, 0}, {"/ten", 5, 100, false, 5}, {"/ten", 10, 0, false, 0},
	} {
		var b []byte
		r, err := fsys.Read(ctx, tc.p, tc.off, tc.n)
		if err == nil {
			b, err = io.ReadAll(r)
			r.Close()
		}
		if (err != nil) != tc.fails || len(b) != tc.wantSize {
			t.Errorf("%s offset %d length %d: %d bytes, %v", tc.p, tc.off, tc.n, len(b), err)
		}
	}
	st, err := mc.StatObject(ctx, bucket, prefix+"/ten", minio.StatObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The client library decodes the encoded form the server returns.
	if got := st.UserMetadata["Place"]; got != "Köln 日本" {
		t.Errorf("value = %q", got)
	}
}

func TestStandardHeaderMetadataSmallPut(t *testing.T) {
	rec, ep := newRecorder(t)
	fsys, _ := openFSAt(t, ep, "")
	md := map[string]string{"content-type": "image/x-custom", "CACHE-CONTROL": "max-age=60", "x-amz-meta-Content-Language": "de", "Source": "camera"}
	if err := writeBytes(t, fsys, "/h.png", []byte("x"), files.WriteOptions{Metadata: md}); err != nil {
		t.Fatal(err)
	}
	puts := rec.matching(func(r *http.Request) bool { return r.Method == http.MethodPut && r.URL.Path == "/"+fakeBucket+"/h.png" })
	if len(puts) != 1 {
		t.Fatalf("%d puts", len(puts))
	}
	h := puts[0].Header
	if h.Get("Content-Type") != "image/x-custom" || h.Get("Cache-Control") != "max-age=60" || h.Get("Content-Language") != "de" ||
		h.Get("X-Amz-Meta-Source") != "camera" || h.Get("X-Amz-Meta-Cache-Control") != "" {
		t.Fatalf("headers = %v", h)
	}
}

// A copy that has to set the metadata again (an object with Expires, or above
// maxCopyObject) keeps another tool's user metadata, also under names of
// standard headers, and values that are not ASCII.
func TestReplacingCopyKeepsForeignMetadataReal(t *testing.T) {
	fsys, mc, bucket := openReal(t)
	ctx := context.Background()
	prefix := drivePrefix(t, fsys, mc, bucket)
	expires := time.Date(2037, 10, 21, 7, 28, 0, 0, time.UTC)
	_, err := mc.PutObject(ctx, bucket, prefix+"/foreign", strings.NewReader("data"), 4, minio.PutObjectOptions{
		ContentType: "text/x-real", CacheControl: "no-store", Expires: expires,
		UserMetadata: map[string]string{
			"x-amz-meta-content-type": "meta-type", "x-amz-meta-cache-control": "meta-cache",
			"Place": "Köln", "Odraz-Sha256": hexSHA([]byte("data")) + ":4",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int64{5 << 30, 1} { // one request, then multipart
		func() {
			defer s3.SetMaxCopyObject(limit)()
			p := fmt.Sprintf("/copy-%d", limit)
			if err := fsys.(files.Copier).Copy(ctx, "/foreign", p, false); err != nil {
				t.Fatalf("limit %d: %v", limit, err)
			}
			st, err := mc.StatObject(ctx, bucket, prefix+p, minio.StatObjectOptions{})
			if err != nil {
				t.Fatal(err)
			}
			place, _ := new(mime.WordDecoder).DecodeHeader(st.UserMetadata["Place"])
			if st.ContentType != "text/x-real" || st.Metadata.Get("Cache-Control") != "no-store" || !st.Expires.Equal(expires) ||
				st.UserMetadata["Content-Type"] != "meta-type" || st.UserMetadata["Cache-Control"] != "meta-cache" || place != "Köln" {
				t.Errorf("limit %d: %q %v %v %q", limit, st.ContentType, st.Metadata, st.UserMetadata, place)
			}
			keeps(t, fsys, p, []byte("data"))
		}()
	}
}
