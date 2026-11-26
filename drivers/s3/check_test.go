package s3_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"net/http/httputil"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/drivers/s3"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
)

func TestPartSizeFor(t *testing.T) {
	for _, size := range []int64{0, 1, 16 << 20, 16<<20 + 1, 150 << 30, 200 << 30, 4 << 40} {
		p := s3.PartSize(size)
		if p < 16<<20 || p%(1<<20) != 0 {
			t.Errorf("%d: part %d is not a whole number of MiB of at least 16", size, p)
		}
		if parts := (size + p - 1) / p; parts > 10000 {
			t.Errorf("%d: %d parts", size, parts)
		}
	}
	if s3.PartSize(1) != 16<<20 {
		t.Errorf("small: %d", s3.PartSize(1))
	}
}

func checkFor(t *testing.T, fsys files.FS, data []byte, size int64) files.Check {
	t.Helper()
	c := fsys.(files.ObjectStore).NewCheck(size)
	for i := 0; i < len(data); i += 70001 { // odd chunks, across part boundaries
		if _, err := c.Write(data[i:min(i+70001, len(data))]); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// Sizes the in-process fake stores faithfully (single PUT of known size).
func TestCheckOnSinglePut(t *testing.T) {
	fsys, _ := openFS(t, "")
	ctx := context.Background()
	for _, n := range []int{0, 1000, 1 << 20} {
		data := patterned(n, 21)
		p := "/c" + strconv.Itoa(n)
		if err := writeBytes(t, fsys, p, data, files.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := checkFor(t, fsys, data, int64(n)).Verify(ctx, p); err != nil {
			t.Errorf("%d: %v", n, err)
		}
		if n == 0 {
			continue
		}
		other := append([]byte(nil), data...)
		other[n-1] ^= 1
		if err := checkFor(t, fsys, other, int64(n)).Verify(ctx, p); !errors.Is(err, odraz.ErrChecksum) {
			t.Errorf("%d: one byte changed: %v", n, err)
		}
		if err := checkFor(t, fsys, data[:n-1], int64(n)).Verify(ctx, p); !errors.Is(err, odraz.ErrChecksum) {
			t.Errorf("%d: short feed: %v", n, err)
		}
		if err := checkFor(t, fsys, data, -1).Verify(ctx, p); !errors.Is(err, odraz.ErrUnsupported) {
			t.Errorf("%d: unknown size: %v", n, err)
		}
	}
	if err := checkFor(t, fsys, []byte("x"), 1).Verify(ctx, "/missing"); !errors.Is(err, odraz.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

// With SSE-KMS the ETag is not an MD5, so it cannot be compared.
func TestCheckKMSUnsupported(t *testing.T) {
	ep := newFakeS3(t)
	u, _ := url.Parse(ep)
	front := httptest.NewServer(&httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(u); r.Out.Host = r.In.Host },
		ModifyResponse: func(res *http.Response) error {
			if res.Request.Method == http.MethodHead && res.StatusCode == 200 {
				res.Header.Set("X-Amz-Server-Side-Encryption", "aws:kms")
			}
			return nil
		},
	})
	t.Cleanup(front.Close)
	fsys, _ := openFSAt(t, front.URL, "")
	if err := fsys.Write(context.Background(), "/k", bytes.NewReader([]byte("data")), files.WriteOptions{Size: 4}); err != nil {
		t.Fatal(err)
	}
	if err := checkFor(t, fsys, []byte("data"), 4).Verify(context.Background(), "/k"); !errors.Is(err, odraz.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

// Multipart objects on a real server, at the part boundaries.
func TestCheckMatchesWhatWasWrittenReal(t *testing.T) {
	fsys, _, _ := openReal(t)
	ctx := context.Background()
	for _, n := range []int{0, 1000, 16 << 20, 16<<20 + 1, 40 << 20} {
		data := patterned(n, 22)
		p := "/c" + strconv.Itoa(n)
		if err := writeBytes(t, fsys, p, data, files.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := checkFor(t, fsys, data, int64(n)).Verify(ctx, p); err != nil {
			t.Errorf("%d: %v", n, err)
		}
		if n > 0 {
			other := append([]byte(nil), data...)
			other[n-1] ^= 1
			if err := checkFor(t, fsys, other, int64(n)).Verify(ctx, p); !errors.Is(err, odraz.ErrChecksum) {
				t.Errorf("%d: last byte changed: %v", n, err)
			}
		}
	}
}
