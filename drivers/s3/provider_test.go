package s3_test

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
	"github.com/odrazdotxyz/odraz-go-sdk/files/fstest"
)

// Over TLS the client library signs and checksums uploads differently than over
// plain HTTP. This runs the conformance suite and a large upload through a TLS
// front (self-signed) of the real test server.
func TestConformanceS3TLS(t *testing.T) {
	ep, _, _, _ := realS3(t)
	if strings.HasPrefix(ep, "https://") {
		t.Skip("the server is already reached over TLS")
	}
	target, err := url.Parse(ep)
	if err != nil {
		t.Fatal(err)
	}
	// The Host header stays as the client signed it.
	front := httptest.NewTLSServer(&httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.Out.Host = r.In.Host
	}})
	t.Cleanup(front.Close)
	t.Setenv("ODRAZ_TEST_S3_ENDPOINT", front.URL)
	t.Setenv("ODRAZ_TEST_S3_TLS_INSECURE", "1")
	if _, err := (&http.Client{}).Get(front.URL); err == nil {
		t.Fatal("the test certificate is trusted; the insecure option would not be exercised")
	}
	t.Run("suite", func(t *testing.T) {
		fstest.Run(t, func(t *testing.T) files.FS {
			fsys, _, _ := openReal(t)
			return fsys
		})
	})
	t.Run("large upload with checksum and headers", func(t *testing.T) {
		fsys, mc, bucket := openReal(t)
		prefix := drivePrefix(t, fsys, mc, bucket)
		for name, size := range map[string]int{"single": 9 << 20, "multi": 20 << 20} {
			data := patterned(size, 11)
			if err := writeBytes(t, fsys, "/"+name, data, files.WriteOptions{Metadata: headerMeta()}); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			checkHeaders(t, fsys, mc, bucket, prefix, "/"+name, data, true)
			keeps(t, fsys, "/"+name, data)
		}
	})
}

// A bucket that keeps old versions (as Backblaze B2 does by default) behaves
// like any other. Skips where a bucket cannot be created or versioned.
func TestConformanceS3Versioned(t *testing.T) {
	_, mc, _ := openReal(t)
	ctx := context.Background()
	bucket := fmt.Sprintf("odraz-versioned-%d", rand.Int63())
	if err := mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Skipf("cannot create a bucket: %v", err)
	}
	t.Cleanup(func() {
		for o := range mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
			if o.Err == nil {
				_ = mc.RemoveObject(ctx, bucket, o.Key, minio.RemoveObjectOptions{VersionID: o.VersionID})
			}
		}
		_ = mc.RemoveBucket(ctx, bucket)
	})
	if err := mc.EnableVersioning(ctx, bucket); err != nil {
		t.Skipf("cannot enable versioning: %v", err)
	}
	t.Setenv("ODRAZ_TEST_S3_BUCKET", bucket)
	fstest.Run(t, func(t *testing.T) files.FS {
		fsys, _, _ := openReal(t)
		return fsys
	})
	// The old content of a replaced object is still there as a version, and a
	// deleted one is gone from the drive.
	fsys, _, _ := openReal(t)
	for _, body := range []string{"first", "second"} {
		if err := writeBytes(t, fsys, "/v.txt", []byte(body), files.WriteOptions{Overwrite: true}); err != nil {
			t.Fatal(err)
		}
	}
	if got := readAll(t, fsys, "/v.txt"); !bytes.Equal(got, []byte("second")) {
		t.Fatalf("read %q", got)
	}
	if err := fsys.Delete(ctx, "/v.txt", false); err != nil {
		t.Fatal(err)
	}
	absent(t, fsys, "/v.txt")
}
