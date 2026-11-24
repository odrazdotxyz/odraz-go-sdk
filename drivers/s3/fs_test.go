package s3_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
)

// openFS opens the drive of a fresh fake and returns it with a direct minio
// client on the same bucket for seeding objects.
func openFS(t *testing.T, prefix string) (files.FS, *minio.Client) {
	t.Helper()
	return openFSAt(t, newFakeS3(t), prefix)
}

func openFSAt(t *testing.T, ep, prefix string) (files.FS, *minio.Client) {
	t.Helper()
	cfg := map[string]string{"endpoint": ep, "bucket": fakeBucket}
	if prefix != "" {
		cfg["prefix"] = prefix
	}
	acc, store := account(t, fakeAccessKey, fakeSecret, cfg)
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
	mc, err := minio.New(ep[len("http://"):], &minio.Options{
		Creds:        credentials.NewStaticV4(fakeAccessKey, fakeSecret, ""),
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	return fsys, mc
}

func put(t *testing.T, mc *minio.Client, key string, data []byte) {
	t.Helper()
	_, err := mc.PutObject(context.Background(), fakeBucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	if err != nil {
		t.Fatalf("put %q: %v", key, err)
	}
}

func TestListPaginates(t *testing.T) {
	fsys, mc := openFS(t, "")
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
			_, err := mc.PutObject(context.Background(), fakeBucket, fmt.Sprintf("big/f%04d", i), bytes.NewReader([]byte("x")), 1, minio.PutObjectOptions{})
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
	got, err := fsys.List(context.Background(), "/big")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range got {
		if e.IsDir || e.Size != 1 {
			t.Fatalf("bad entry %+v", e)
		}
		seen[e.Path] = true
	}
	if len(got) != n || len(seen) != n {
		t.Fatalf("got %d entries, %d distinct, want %d", len(got), len(seen), n)
	}
}

func TestFileAndFolderSameName(t *testing.T) {
	fsys, mc := openFS(t, "")
	ctx := context.Background()
	put(t, mc, "a", []byte("file"))
	put(t, mc, "a/x", []byte("child"))
	e, err := fsys.Stat(ctx, "/a")
	if err != nil || e.IsDir || e.Size != 4 {
		t.Fatalf("Stat = %+v, %v; want the file", e, err)
	}
	if _, err := fsys.List(ctx, "/a"); !errors.Is(err, odraz.ErrInvalidPath) {
		t.Fatalf("List err = %v, want ErrInvalidPath", err)
	}
}

func TestRangeRead(t *testing.T) {
	fsys, mc := openFS(t, "")
	ctx := context.Background()
	data := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	put(t, mc, "r.txt", data)
	read := func(off, n int64) string {
		rc, err := fsys.Read(ctx, "/r.txt", off, n)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := read(10, 5); got != "abcde" {
		t.Errorf("range = %q", got)
	}
	if got := read(10, -1); got != string(data[10:]) {
		t.Errorf("to end = %q", got)
	}
	if got := read(0, -1); got != string(data) {
		t.Errorf("whole = %q", got)
	}
	if got := read(30, 100); got != string(data[30:]) {
		t.Errorf("past end = %q", got)
	}
}

func TestReadFolderIsInvalidPath(t *testing.T) {
	fsys, mc := openFS(t, "")
	put(t, mc, "dir/x", []byte("x"))
	if _, err := fsys.Read(context.Background(), "/dir", 0, -1); !errors.Is(err, odraz.ErrInvalidPath) {
		t.Fatalf("err = %v, want ErrInvalidPath", err)
	}
}

func TestStatMissing(t *testing.T) {
	fsys, _ := openFS(t, "")
	ctx := context.Background()
	if _, err := fsys.Stat(ctx, "/nope"); !errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("Stat err = %v", err)
	}
	if _, err := fsys.List(ctx, "/nope"); !errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("List err = %v", err)
	}
	if _, err := fsys.Read(ctx, "/nope", 0, -1); !errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("Read err = %v", err)
	}
	if e, err := fsys.Stat(ctx, "/"); err != nil || !e.IsDir {
		t.Fatalf("root Stat = %+v, %v", e, err)
	}
}

func TestUnicodeAndSpecialNames(t *testing.T) {
	for _, prefix := range []string{"", "pre/fix"} {
		fsys, mc := openFS(t, prefix)
		ctx := context.Background()
		base := ""
		if prefix != "" {
			base = prefix + "/"
		}
		names := []string{"é/日本.png", "a+b/%41#?.txt", "sp ace/b c.jpg"}
		for _, n := range names {
			put(t, mc, base+n, []byte("data:"+n))
		}
		root, err := fsys.List(ctx, "/")
		if err != nil {
			t.Fatal(err)
		}
		dirs := map[string]bool{}
		for _, e := range root {
			if !e.IsDir || e.Size != 0 {
				t.Fatalf("root entry %+v", e)
			}
			dirs[e.Name] = true
		}
		if len(dirs) != 3 || !dirs["é"] || !dirs["a+b"] || !dirs["sp ace"] {
			t.Fatalf("root = %v", dirs)
		}
		for _, n := range names {
			p := "/" + n
			e, err := fsys.Stat(ctx, p)
			if err != nil || e.IsDir || e.Path != p || e.ModTime.IsZero() || e.ETag == "" {
				t.Fatalf("Stat(%q) = %+v, %v", p, e, err)
			}
			rc, err := fsys.Read(ctx, p, 0, -1)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(rc)
			rc.Close()
			if string(b) != "data:"+n {
				t.Fatalf("Read(%q) = %q", p, b)
			}
		}
		kids, err := fsys.List(ctx, "/a+b")
		if err != nil || len(kids) != 1 || kids[0].Path != "/a+b/%41#?.txt" || kids[0].Name != "%41#?.txt" {
			t.Fatalf("List = %+v, %v", kids, err)
		}
	}
}

// gofakes3 trims slashes from request paths, so it can neither store nor
// serve a key ending in "/" and rejects zero-byte PUTs from minio. This stub
// stands in for a bucket holding only the folder marker "empty/".
func markerServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Method != http.MethodGet || q.Get("list-type") != "2" {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			if r.Method != http.MethodHead {
				io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`)
			}
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		body := `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>test</Name><IsTruncated>false</IsTruncated>`
		switch q.Get("prefix") {
		case "":
			body += `<CommonPrefixes><Prefix>empty/</Prefix></CommonPrefixes>`
		case "empty/":
			body += `<Contents><Key>empty/</Key><LastModified>2026-01-02T03:04:05.000Z</LastModified><ETag>"d41d8cd98f00b204e9800998ecf8427e"</ETag><Size>0</Size></Contents>`
		}
		io.WriteString(w, body+`</ListBucketResult>`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestFolderMarkers(t *testing.T) {
	fsys, _ := openFSAt(t, markerServer(t), "")
	ctx := context.Background()
	e, err := fsys.Stat(ctx, "/empty")
	if err != nil || !e.IsDir || e.Size != 0 || e.ModTime.Year() != 2026 {
		t.Fatalf("Stat marker folder = %+v, %v", e, err)
	}
	kids, err := fsys.List(ctx, "/empty")
	if err != nil || len(kids) != 0 {
		t.Fatalf("List = %+v, %v", kids, err)
	}
	root, _ := fsys.List(ctx, "/")
	if len(root) != 1 || root[0].Path != "/empty" || !root[0].IsDir || !root[0].ModTime.IsZero() {
		t.Fatalf("root = %+v", root)
	}
	if _, err := fsys.Read(ctx, "/empty", 0, -1); !errors.Is(err, odraz.ErrInvalidPath) {
		t.Fatalf("Read err = %v", err)
	}
}

func TestCapabilities(t *testing.T) {
	fsys, _ := openFS(t, "")
	want := files.Capabilities{RangeRead: true, ServerSideCopy: true}
	if got := fsys.Capabilities(); got != want {
		t.Fatalf("caps = %+v", got)
	}
}
