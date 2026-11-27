package nextcloud_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
)

// etagServer holds one file and, like Nextcloud, refuses a DELETE whose
// If-Match is not the file's ETag.
func etagServer(t *testing.T, etag *string, gone *bool, sent *[]string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dir := strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/d")
		if r.URL.Path == "/ocs/v2.php/cloud/user" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ocs":{"data":{"id":"alice","displayname":"Alice"}}}`)
			return
		}
		switch r.Method {
		case "PROPFIND":
			kind := ""
			if dir {
				kind = "<d:collection/>"
			}
			w.WriteHeader(http.StatusMultiStatus)
			fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:href>%s</d:href><d:propstat><d:prop><d:resourcetype>%s</d:resourcetype><d:getetag>%s</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, r.URL.Path, kind, strings.ReplaceAll(*etag, `"`, "&quot;"))
		case http.MethodDelete:
			*sent = append(*sent, r.Header.Get("If-Match"))
			if r.Header.Get("If-Match") != *etag {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			*gone = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDeleteIfMatch(t *testing.T) {
	etag, gone, sent := `"v2"`, false, []string(nil)
	srv := etagServer(t, &etag, &gone, &sent)
	fsys, err := openClient(t, srv.URL, "alice", "x").OpenDrive(ctx, "personal")
	if err != nil {
		t.Fatal(err)
	}
	del := fsys.(files.ConditionalDeleter)
	if err := del.DeleteIfMatch(ctx, "/f.jpg", `"v1"`); !errors.Is(err, odraz.ErrChanged) || gone {
		t.Fatalf("stale: %v, gone %v", err, gone)
	}
	if err := del.DeleteIfMatch(ctx, "/d", `"v2"`); !errors.Is(err, odraz.ErrInvalidPath) || gone {
		t.Fatalf("folder: %v", err)
	}
	st, err := fsys.Stat(ctx, "/f.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := del.DeleteIfMatch(ctx, "/f.jpg", st.ETag); err != nil || !gone {
		t.Fatalf("current: %v", err)
	}
	gone = false
	if err := del.DeleteIfMatch(ctx, "/f.jpg", "v2"); err != nil || sent[len(sent)-1] != `"v2"` {
		t.Fatalf("unquoted etag: %v, sent %q", err, sent)
	}
}

func TestRealDeleteIfMatch(t *testing.T) {
	url := realURL(t)
	fsys, err := openClient(t, url, "alice", "alice-pass-123").OpenDrive(ctx, "personal")
	if err != nil {
		t.Fatal(err)
	}
	const p = "/odraz-delete-if-match.txt"
	t.Cleanup(func() { _ = fsys.Delete(ctx, p, false) })
	put := func(s string) files.Entry {
		if err := fsys.Write(ctx, p, strings.NewReader(s), files.WriteOptions{Size: int64(len(s)), Overwrite: true}); err != nil {
			t.Fatal(err)
		}
		e, err := fsys.Stat(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	old := put("one")
	cur := put("second version")
	del := fsys.(files.ConditionalDeleter)
	if err := del.DeleteIfMatch(ctx, p, old.ETag); !errors.Is(err, odraz.ErrChanged) {
		t.Fatalf("stale: %v", err)
	}
	if _, err := fsys.Stat(ctx, p); err != nil {
		t.Fatalf("removed by a stale delete: %v", err)
	}
	if err := del.DeleteIfMatch(ctx, p, cur.ETag); err != nil {
		t.Fatalf("current: %v", err)
	}
	if _, err := fsys.Stat(ctx, p); !errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("still there: %v", err)
	}
}
