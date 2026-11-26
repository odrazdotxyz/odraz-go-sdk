package s3_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
)

// Backblaze B2 refuses a request with a non-ASCII byte in a header (the
// signature check fails), so metadata goes out RFC 2047-encoded.
func TestMetadataIsSentAsASCII(t *testing.T) {
	rec, ep := newRecorder(t)
	fsys, _ := openFSAt(t, ep, "")
	err := writeBytes(t, fsys, "/a", []byte("x"), files.WriteOptions{Metadata: map[string]string{"place": "Köln 日本", "long": strings.Repeat("é ", 100), "plain": "as is"}})
	if err != nil {
		t.Fatal(err)
	}
	puts := rec.matching(func(r *http.Request) bool { return r.Header.Get("X-Amz-Meta-Place") != "" })
	if len(puts) != 1 {
		t.Fatalf("%d requests carry the metadata", len(puts))
	}
	for k, vs := range puts[0].Header {
		for _, v := range vs {
			for i := 0; i < len(v); i++ {
				if v[i] >= 0x80 {
					t.Fatalf("header %s has a non-ASCII byte: %q", k, v)
				}
			}
		}
	}
	if got := puts[0].Header.Get("X-Amz-Meta-Plain"); got != "as is" {
		t.Errorf("plain = %q", got)
	}
}

// A standard header cannot carry encoded words: refused before anything is sent.
func TestNonASCIIStandardHeaderIsRefused(t *testing.T) {
	fsys, _ := openFS(t, "")
	err := fsys.Write(context.Background(), "/a", strings.NewReader("x"), files.WriteOptions{Size: 1, Metadata: map[string]string{"Content-Disposition": `attachment; filename="Köln.txt"`}})
	if !errors.Is(err, odraz.ErrInvalidPath) || !strings.Contains(err.Error(), "ASCII") {
		t.Fatalf("err = %v", err)
	}
}
