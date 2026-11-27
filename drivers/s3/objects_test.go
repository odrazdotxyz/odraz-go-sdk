package s3_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/minio/minio-go/v7"
	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
)

func purge(t *testing.T, fsys files.FS, p string) error {
	t.Helper()
	return fsys.(files.ObjectStore).Purge(context.Background(), p)
}

// versionedDrive opens a drive on a bucket that keeps versions: the test bucket
// if it already does, else a new one. It skips where there is no such thing.
func versionedDrive(t *testing.T) (files.FS, *minio.Client, string, string) {
	t.Helper()
	fsys, mc, bucket := openReal(t)
	ctx := context.Background()
	if v, err := mc.GetBucketVersioning(ctx, bucket); err != nil || !v.Enabled() {
		bucket = fmt.Sprintf("odraz-purge-%d", rand.Int63())
		if err := mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			t.Skipf("no versioned bucket and cannot create one: %v", err)
		}
		name := bucket
		t.Cleanup(func() {
			for o := range mc.ListObjects(ctx, name, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
				if o.Err == nil {
					_ = mc.RemoveObject(ctx, name, o.Key, minio.RemoveObjectOptions{VersionID: o.VersionID})
				}
			}
			_ = mc.RemoveBucket(ctx, name)
		})
		if err := mc.EnableVersioning(ctx, bucket); err != nil {
			t.Skipf("cannot enable versioning: %v", err)
		}
		t.Setenv("ODRAZ_TEST_S3_BUCKET", bucket)
		fsys, mc, bucket = openReal(t)
	}
	return fsys, mc, bucket, drivePrefix(t, fsys, mc, bucket)
}

// versions counts the versions and delete markers of exactly key.
func versions(t *testing.T, mc *minio.Client, bucket, key string) (n int) {
	t.Helper()
	for o := range mc.ListObjects(context.Background(), bucket, minio.ListObjectsOptions{Prefix: key, Recursive: true, WithVersions: true}) {
		if o.Err != nil {
			t.Fatal(o.Err)
		}
		if o.Key == key {
			n++
		}
	}
	return n
}

func full(prefix, p string) string { return strings.TrimSuffix(prefix, "/") + p }

func TestPurgeRemovesAllVersionsReal(t *testing.T) {
	fsys, mc, bucket, prefix := versionedDrive(t)
	ctx := context.Background()
	for _, body := range []string{"one", "two", "three"} {
		if err := writeBytes(t, fsys, "/p.txt", []byte(body), files.WriteOptions{Overwrite: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := fsys.Delete(ctx, "/p.txt", false); err != nil {
		t.Fatal(err)
	}
	if n := versions(t, mc, bucket, full(prefix, "/p.txt")); n < 3 {
		t.Fatalf("only %d versions before the purge: the bucket does not keep them", n)
	}
	if err := purge(t, fsys, "/p.txt"); err != nil {
		t.Fatal(err)
	}
	if n := versions(t, mc, bucket, full(prefix, "/p.txt")); n != 0 {
		t.Fatalf("%d versions or markers left", n)
	}
}

func TestPurgeLeavesNeighboursReal(t *testing.T) {
	fsys, _, _ := openReal(t)
	for _, p := range []string{"/a", "/ab", "/a.txt"} {
		if err := writeBytes(t, fsys, p, []byte(p), files.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := purge(t, fsys, "/a"); err != nil {
		t.Fatal(err)
	}
	absent(t, fsys, "/a")
	for _, p := range []string{"/ab", "/a.txt"} {
		if got := string(readAll(t, fsys, p)); got != p {
			t.Errorf("%s = %q", p, got)
		}
	}
}

func TestPurgeEmptyFolderMarkerReal(t *testing.T) {
	fsys, _, _ := openReal(t)
	ctx := context.Background()
	if err := fsys.Mkdir(ctx, "/d/e"); err != nil {
		t.Fatal(err)
	}
	if err := writeBytes(t, fsys, "/d/e/f", []byte("x"), files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := purge(t, fsys, "/d/e/f"); err != nil {
		t.Fatal(err)
	}
	if err := purge(t, fsys, "/d/e"); err != nil {
		t.Fatal(err)
	}
	absent(t, fsys, "/d/e")
	if _, err := fsys.Stat(ctx, "/d"); err != nil {
		t.Errorf("the parent went too: %v", err)
	}
}

// On a bucket that keeps versions an emptied folder leaves nothing of its marker.
func TestPurgeEmptyFolderVersionsReal(t *testing.T) {
	fsys, mc, bucket, prefix := versionedDrive(t)
	if err := fsys.Mkdir(context.Background(), "/d"); err != nil {
		t.Fatal(err)
	}
	if err := purge(t, fsys, "/d"); err != nil {
		t.Fatal(err)
	}
	if n := versions(t, mc, bucket, full(prefix, "/d/")); n != 0 {
		t.Fatalf("%d versions of the folder marker left", n)
	}
	absent(t, fsys, "/d")
}

func TestPurgeFolderWithFilesIsNotEmptyReal(t *testing.T) {
	fsys, _, _ := openReal(t)
	if err := fsys.Mkdir(context.Background(), "/d"); err != nil {
		t.Fatal(err)
	}
	if err := writeBytes(t, fsys, "/d/f", []byte("keep"), files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := purge(t, fsys, "/d"); !errors.Is(err, odraz.ErrNotEmpty) {
		t.Fatalf("err = %v", err)
	}
	if got := string(readAll(t, fsys, "/d/f")); got != "keep" {
		t.Fatalf("got %q", got)
	}
}

func TestPurgeMissingIsNil(t *testing.T) {
	fsys, _ := openFS(t, "")
	if err := purge(t, fsys, "/nothing/here"); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeRootIsInvalid(t *testing.T) {
	fsys, _ := openFS(t, "")
	if err := purge(t, fsys, "/"); !errors.Is(err, odraz.ErrInvalidPath) {
		t.Fatalf("err = %v", err)
	}
}

// A server without version listing gets a plain delete.
func TestPurgeUnversioned(t *testing.T) {
	fsys, _ := openFS(t, "")
	if err := writeBytes(t, fsys, "/u", []byte("x"), files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := purge(t, fsys, "/u"); err != nil {
		t.Fatal(err)
	}
	absent(t, fsys, "/u")
}

func TestPurgeRefusedDeleteIsAnError(t *testing.T) {
	u, _ := url.Parse(newFakeS3(t))
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(u); r.Out.Host = r.In.Host }}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete || r.URL.Query().Has("delete") {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `<Error><Code>AccessDenied</Code><Message>no</Message></Error>`)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	fsys, _ := openFSAt(t, front.URL, "")
	if err := writeBytes(t, fsys, "/locked", []byte("x"), files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := purge(t, fsys, "/locked"); !errors.Is(err, odraz.ErrPermission) {
		t.Fatalf("err = %v", err)
	}
}

// Nothing says every server lists versions in key order: a longer key listed
// first must not hide the versions of the key being purged.
func TestPurgeUnsortedListing(t *testing.T) {
	version := func(key, id string) string {
		return "<Version><Key>" + key + "</Key><VersionId>" + id + "</VersionId><IsLatest>false</IsLatest><LastModified>2026-01-01T00:00:00Z</LastModified><Size>1</Size></Version>"
	}
	var mu sync.Mutex
	var removed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case r.URL.Query().Has("location"):
			fmt.Fprint(w, `<LocationConstraint></LocationConstraint>`)
		case r.URL.Query().Has("versions"):
			fmt.Fprint(w, `<ListVersionsResult><Name>test</Name><Prefix>key</Prefix><IsTruncated>false</IsTruncated>`+
				version("keyz", "z1")+version("key", "v1")+version("keyz", "z2")+version("key", "v2")+`</ListVersionsResult>`)
		case r.Method == http.MethodDelete:
			mu.Lock()
			removed = append(removed, strings.TrimPrefix(r.URL.Path, "/test/")+"@"+r.URL.Query().Get("versionId"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	fsys, _ := openFSAt(t, srv.URL, "")
	if err := purge(t, fsys, "/key"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{"key@v1", "key@v2"}) {
		t.Fatalf("removed %v", removed)
	}
}
