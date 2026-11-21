package dav

import (
	"context"
	"errors"
	"os"
	"testing"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
)

// Runs against testenv/webdav: ODRAZ_TEST_WEBDAV_URL=http://localhost:8081/dav
func TestRealServer(t *testing.T) {
	url := os.Getenv("ODRAZ_TEST_WEBDAV_URL")
	if url == "" {
		t.Skip("ODRAZ_TEST_WEBDAV_URL not set")
	}
	ctx := context.Background()
	creds := accounts.StaticProvider{Creds: accounts.Credentials{Kind: accounts.CredBasic, Username: "odraz", Password: "odraz-test"}}
	c, err := New(Config{BaseURL: url, Creds: creds})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := c.Propfind(ctx, "/", 1)
	if err != nil || len(rs) == 0 || rs[0].Path != "/" || !rs[0].IsDir {
		t.Fatalf("root: %+v %v", rs, err)
	}
	if _, err := c.Propfind(ctx, "/does-not-exist", 0); !errors.Is(err, odraz.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	bad, _ := New(Config{BaseURL: url, Creds: accounts.StaticProvider{Creds: accounts.Credentials{Kind: accounts.CredBasic, Username: "x", Password: "y"}}})
	if _, err := bad.Propfind(ctx, "/", 0); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("bad creds: %v", err)
	}
}
