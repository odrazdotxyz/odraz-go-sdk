package davfs

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/internal/dav"
)

func newFS(t *testing.T, h http.HandlerFunc) *FS {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := dav.New(dav.Config{BaseURL: srv.URL, Creds: accounts.StaticProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	return &FS{C: c}
}

// Nextcloud answers "Range: bytes=0-0" with a 206 that carries the whole file.
func TestReadTrimsWhenServerReturnsMoreThanAsked(t *testing.T) {
	fs := newFS(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-9/10")
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, "0123456789")
	})
	for _, c := range []struct {
		off, n int64
		want   string
	}{{0, 1, "0"}, {3, 4, "3456"}, {0, -1, "0123456789"}} {
		rc, err := fs.Read(context.Background(), "/f", c.off, c.n)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if string(b) != c.want {
			t.Errorf("Read(%d,%d) = %q, want %q", c.off, c.n, b, c.want)
		}
	}
}

// A correct 206 is passed through untouched.
func TestReadHonoursCorrectRange(t *testing.T) {
	fs := newFS(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 3-6/10")
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, "3456")
	})
	rc, err := fs.Read(context.Background(), "/f", 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	if string(b) != "3456" {
		t.Errorf("got %q", b)
	}
}

// Nextcloud answers a GET on a folder with an HTML listing and status 200.
func TestReadFolderServedAsHTML(t *testing.T) {
	fs := newFS(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PROPFIND" {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(207)
			io.WriteString(w, `<multistatus xmlns="DAV:"><response><href>/d/</href><propstat><prop><resourcetype><collection/></resourcetype></prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, "<html>listing</html>")
	})
	if _, err := fs.Read(context.Background(), "/d", 0, -1); !errors.Is(err, odraz.ErrInvalidPath) {
		t.Errorf("Read of a folder = %v, want ErrInvalidPath", err)
	}
}

// A real HTML file must still be readable.
func TestReadHTMLFile(t *testing.T) {
	fs := newFS(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PROPFIND" {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(207)
			io.WriteString(w, `<multistatus xmlns="DAV:"><response><href>/p.html</href><propstat><prop><resourcetype/></prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<p>hi</p>")
	})
	rc, err := fs.Read(context.Background(), "/p.html", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	if string(b) != "<p>hi</p>" {
		t.Errorf("got %q", b)
	}
}
