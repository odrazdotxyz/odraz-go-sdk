package webdav_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	_ "github.com/odrazdotxyz/odraz-go-sdk/drivers/webdav"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
	"github.com/odrazdotxyz/odraz-go-sdk/files/fstest"
	xwebdav "golang.org/x/net/webdav"
)

func openFS(t *testing.T, url, user, pass string) files.FS {
	t.Helper()
	ctx := context.Background()
	store := accounts.NewMemoryStore()
	store.Put(ctx, "acc", accounts.SecretUsername, []byte(user))
	store.Put(ctx, "acc", accounts.SecretPassword, []byte(pass))
	acc := accounts.Account{ID: "acc", Label: "Test", Adapter: "webdav", Config: map[string]string{"url": url}}
	h, err := accounts.Open(ctx, acc, store)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := files.ForAccount(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	drives, err := cl.Drives(ctx)
	if err != nil || len(drives) != 1 {
		t.Fatalf("drives = %v, %v", drives, err)
	}
	fs, err := cl.OpenDrive(ctx, drives[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

// Fast, always-on smoke test against golang.org/x/net/webdav.
func TestConformanceFakeServer(t *testing.T) {
	srv := httptest.NewServer(&xwebdav.Handler{Prefix: "/dav", FileSystem: xwebdav.NewMemFS(), LockSystem: xwebdav.NewMemLS()})
	defer srv.Close()
	fstest.Run(t, func(t *testing.T) files.FS { return openFS(t, srv.URL+"/dav", "u", "p") })
}

// Real server: ODRAZ_TEST_WEBDAV_URL=http://localhost:8081/dav (testenv/webdav/seed.sh).
func TestConformanceApache(t *testing.T) {
	url := os.Getenv("ODRAZ_TEST_WEBDAV_URL")
	if url == "" {
		t.Skip("ODRAZ_TEST_WEBDAV_URL not set")
	}
	fstest.Run(t, func(t *testing.T) files.FS { return openFS(t, url, "odraz", "odraz-test") })
}

func TestValidateAndErrors(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, p, _ := r.BasicAuth(); p != "good" {
			w.WriteHeader(401)
			return
		}
		(&xwebdav.Handler{Prefix: "", FileSystem: xwebdav.NewMemFS(), LockSystem: xwebdav.NewMemLS()}).ServeHTTP(w, r)
	}))
	defer srv.Close()
	mk := func(pass, path string) error {
		st := accounts.NewMemoryStore()
		st.Put(ctx, "a", accounts.SecretUsername, []byte("u"))
		st.Put(ctx, "a", accounts.SecretPassword, []byte(pass))
		return accounts.Validate(ctx, accounts.Account{ID: "a", Adapter: "webdav", Config: map[string]string{"url": srv.URL + path}}, st)
	}
	if err := mk("good", ""); err != nil {
		t.Errorf("valid account: %v", err)
	}
	if err := mk("bad", ""); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("bad password: %v", err)
	}
	if err := mk("good", "/no/such/dir"); !errors.Is(err, odraz.ErrNotFound) {
		t.Errorf("bad path: %v", err)
	}
}

func TestUnsupportedDomain(t *testing.T) {
	_, err := accounts.Open(context.Background(), accounts.Account{ID: "x", Adapter: "nope"}, nil)
	if !errors.Is(err, odraz.ErrUnsupported) {
		t.Errorf("unknown adapter: %v", err)
	}
}
