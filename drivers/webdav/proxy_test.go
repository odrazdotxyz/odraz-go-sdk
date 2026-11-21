package webdav_test

import (
	"context"
	"errors"
	"os"
	"testing"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
	"github.com/odrazdotxyz/odraz-go-sdk/files/fstest"
)

// Needs testenv/proxy: eval "$(testenv/proxy/seed.sh)"
func proxyURLs(t *testing.T) map[string]string {
	u := map[string]string{
		"cross-host redirect": os.Getenv("ODRAZ_TEST_PROXY_URL"),
		"same-host redirect":  os.Getenv("ODRAZ_TEST_PROXY_SAMEHOST_URL"),
		"html 200":            os.Getenv("ODRAZ_TEST_PROXY_HTML_URL"),
	}
	for _, v := range u {
		if v == "" {
			t.Skip("ODRAZ_TEST_PROXY_* not set")
		}
	}
	return u
}

// account builds an account through the same path a UI uses (forms).
func account(t *testing.T, store accounts.SecretStore, url string, headers map[string]string) accounts.Account {
	t.Helper()
	vals := map[string]string{"url": url, "username": "odraz", "password": "odraz-test"}
	for k, v := range headers {
		vals[k] = v
	}
	acc, err := accounts.Configure(context.Background(), store, accounts.Account{ID: "px", Adapter: "webdav"}, "basic", vals)
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

func TestProxyBlocksWithoutHeader(t *testing.T) {
	ctx := context.Background()
	for mode, url := range proxyURLs(t) {
		for name, headers := range map[string]map[string]string{
			"no header":    nil,
			"wrong header": {"header.X-Proxy-Token": "nope"},
		} {
			store := accounts.NewMemoryStore()
			err := accounts.Validate(ctx, account(t, store, url, headers), store)
			if !errors.Is(err, odraz.ErrBlockedByProxy) {
				t.Errorf("%s / %s: err = %v, want ErrBlockedByProxy", mode, name, err)
			} else {
				t.Logf("%s / %s: %v", mode, name, err)
			}
		}
	}
}

func TestProxyAllowsWithHeader(t *testing.T) {
	ctx := context.Background()
	for mode, url := range proxyURLs(t) {
		for name, headers := range map[string]map[string]string{
			"plain header":  {"header.X-Proxy-Token": "secret"},
			"secret header": {"secret_header.X-Proxy-Token": "secret"},
		} {
			store := accounts.NewMemoryStore()
			acc := account(t, store, url, headers)
			if name == "secret header" {
				if v := acc.Config["header.X-Proxy-Token"]; v != "" {
					t.Errorf("secret header value leaked into Account.Config: %q", v)
				}
			}
			if err := accounts.Validate(ctx, acc, store); err != nil {
				t.Errorf("%s / %s: %v", mode, name, err)
			}
		}
	}
}

func TestConformanceThroughProxy(t *testing.T) {
	url := proxyURLs(t)["cross-host redirect"]
	fstest.Run(t, func(t *testing.T) files.FS {
		ctx := context.Background()
		store := accounts.NewMemoryStore()
		h, err := accounts.Open(ctx, account(t, store, url, map[string]string{"secret_header.X-Proxy-Token": "secret"}), store)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := files.ForAccount(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		fs, err := cl.OpenDrive(ctx, "default")
		if err != nil {
			t.Fatal(err)
		}
		return fs
	})
}
