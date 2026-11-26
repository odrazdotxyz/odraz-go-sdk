package s3_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
)

func hexSum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// A caller-supplied hash is verified while streaming: a mismatch stores nothing and keeps what was there.
func TestWriteExpectedSHA256Small(t *testing.T) {
	fsys, _ := openFS(t, "")
	ctx := context.Background()
	body := []byte("the original photo bytes")
	// (Unknown sizes go to a real server below: the fake stores the chunk framing.)
	for name, size := range map[string]int64{"known": int64(len(body))} {
		p := "/ck-" + name
		if err := writeBytes(t, fsys, p, []byte("old"), files.WriteOptions{Size: 3}); err != nil {
			t.Fatal(err)
		}
		err := fsys.Write(ctx, p, bytes.NewReader(body), files.WriteOptions{Size: size, Overwrite: true, SHA256: hexSum([]byte("something else"))})
		if !errors.Is(err, odraz.ErrChecksum) {
			t.Fatalf("%s: mismatch: err = %v, want ErrChecksum", name, err)
		}
		if got := string(readAll(t, fsys, p)); got != "old" {
			t.Errorf("%s: the previous object was replaced: %q", name, got)
		}
		if err := fsys.Write(ctx, p, bytes.NewReader(body), files.WriteOptions{Size: size, Overwrite: true, SHA256: strings.ToUpper(hexSum(body))}); err != nil {
			t.Fatalf("%s: matching hash (any case): %v", name, err)
		}
		if got := string(readAll(t, fsys, p)); got != string(body) {
			t.Errorf("%s: stored %q", name, got)
		}
	}
}

func TestWriteMalformedSHA256IsRefusedBeforeReading(t *testing.T) {
	fsys, _ := openFS(t, "")
	for _, bad := range []string{"abc", strings.Repeat("g", 64), strings.Repeat("a", 63)} {
		r := &countingReader{r: strings.NewReader("x")}
		err := fsys.Write(context.Background(), "/m", r, files.WriteOptions{Size: 1, SHA256: bad})
		if !errors.Is(err, odraz.ErrChecksum) || r.n != 0 {
			t.Errorf("%q: err=%v, bytes read=%d", bad, err, r.n)
		}
	}
}

// Large uploads: the supplied hash is verified and stored; without one nothing is stored and no second copy is made.
func TestLargeUploadChecksumReal(t *testing.T) {
	fsys, _, _ := openReal(t)
	ctx := context.Background()
	hasher := fsys.(files.Hasher)
	for _, n := range []int{9 << 20, 20 << 20} {
		body := bytes.Repeat([]byte{'p'}, n)
		if err := writeBytes(t, fsys, "/big", []byte("old"), files.WriteOptions{Size: 3, Overwrite: true}); err != nil {
			t.Fatal(err)
		}
		bad := append([]byte(nil), body...)
		bad[n-1] = 'q' // differs in the very last byte
		err := fsys.Write(ctx, "/big", bytes.NewReader(bad), files.WriteOptions{Size: int64(n), Overwrite: true, SHA256: hexSum(body)})
		if !errors.Is(err, odraz.ErrChecksum) {
			t.Fatalf("%d: mismatch: %v", n, err)
		}
		if got := string(readAll(t, fsys, "/big")); got != "old" {
			t.Fatalf("%d: a mismatching upload replaced the object (%d bytes now)", n, len(got))
		}
		if err := fsys.Write(ctx, "/big", bytes.NewReader(body), files.WriteOptions{Size: int64(n), Overwrite: true, SHA256: hexSum(body)}); err != nil {
			t.Fatalf("%d: %v", n, err)
		}
		if got, err := hasher.SHA256(ctx, "/big"); err != nil || got != hexSum(body) {
			t.Errorf("%d: stored hash %q, %v", n, got, err)
		}
		if err := fsys.Write(ctx, "/big", bytes.NewReader(body), files.WriteOptions{Size: int64(n), Overwrite: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := hasher.SHA256(ctx, "/big"); !errors.Is(err, odraz.ErrUnsupported) {
			t.Errorf("%d: without a supplied hash a large upload stores none, got %v", n, err)
		}
	}
	// Unknown size: verified, nothing stored (the size is part of the stored value).
	body := bytes.Repeat([]byte{'u'}, 20<<20)
	if err := fsys.Write(ctx, "/unk", io.MultiReader(bytes.NewReader(body)), files.WriteOptions{Size: -1, SHA256: hexSum(body[1:])}); !errors.Is(err, odraz.ErrChecksum) {
		t.Fatalf("unknown size mismatch: %v", err)
	}
	if _, err := fsys.Stat(ctx, "/unk"); !errors.Is(err, odraz.ErrNotFound) {
		t.Errorf("unknown size mismatch left an object: %v", err)
	}
	if err := fsys.Write(ctx, "/unk", io.MultiReader(bytes.NewReader(body)), files.WriteOptions{Size: -1, SHA256: hexSum(body)}); err != nil {
		t.Fatalf("unknown size, matching: %v", err)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += n
	return n, err
}
