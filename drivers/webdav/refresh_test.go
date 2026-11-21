package webdav_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
	xwebdav "golang.org/x/net/webdav"
)

// revokingServer accepts only the bearer token currently in *valid and counts 401s.
func revokingServer(t *testing.T, valid *atomic.Value, unauthorized *atomic.Int32) *httptest.Server {
	h := &xwebdav.Handler{FileSystem: xwebdav.NewMemFS(), LockSystem: xwebdav.NewMemLS()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+valid.Load().(string) {
			unauthorized.Add(1)
			w.WriteHeader(401)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type rig struct {
	srv       *httptest.Server
	store     *accounts.MemoryStore
	acc       accounts.Account
	refreshes atomic.Int32
	valid     atomic.Value
	denied    atomic.Int32
}

// newRig stores an access token that is NOT expired but already revoked server-side.
func newRig(t *testing.T, refreshToken string) *rig {
	r := &rig{store: accounts.NewMemoryStore()}
	r.valid.Store("fresh-token")
	r.srv = revokingServer(t, &r.valid, &r.denied)
	ctx := context.Background()
	r.store.Put(ctx, "a", accounts.SecretAccessToken, []byte("revoked-token"))
	if refreshToken != "" {
		r.store.Put(ctx, "a", accounts.SecretRefreshToken, []byte(refreshToken))
	}
	r.store.Put(ctx, "a", accounts.SecretExpiry, []byte(time.Now().Add(time.Hour).UTC().Format(time.RFC3339)))
	r.acc = accounts.Account{ID: "a", Adapter: "webdav", Config: map[string]string{"url": r.srv.URL}}
	return r
}

func (r *rig) open(t *testing.T, refresh accounts.RefreshFunc) files.FS {
	t.Helper()
	ctx := context.Background()
	h, err := accounts.Open(ctx, r.acc, r.store, accounts.WithRefresh(refresh))
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
}

func (r *rig) goodRefresh(context.Context, accounts.Credentials) (accounts.Credentials, error) {
	r.refreshes.Add(1)
	return accounts.Credentials{Kind: accounts.CredBearer, AccessToken: "fresh-token", Expiry: time.Now().Add(time.Hour)}, nil
}

func TestForcedRefreshOn401(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, "rt")
	fs := r.open(t, r.goodRefresh)
	if _, err := fs.Stat(ctx, "/"); err != nil {
		t.Fatalf("a revoked token must be recovered transparently: %v", err)
	}
	if r.refreshes.Load() != 1 || r.denied.Load() != 1 {
		t.Errorf("refreshes=%d denied=%d, want 1 and 1", r.refreshes.Load(), r.denied.Load())
	}
	if v, _ := r.store.Get(ctx, "a", accounts.SecretAccessToken); string(v) != "fresh-token" {
		t.Errorf("refreshed token not saved: %q", v)
	}
	if _, err := fs.Stat(ctx, "/"); err != nil || r.refreshes.Load() != 1 {
		t.Error("later requests must use the saved token without refreshing")
	}
}

func TestForcedRefreshIsSingleFlight(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, "rt")
	fs := r.open(t, func(c context.Context, o accounts.Credentials) (accounts.Credentials, error) {
		time.Sleep(50 * time.Millisecond)
		return r.goodRefresh(c, o)
	})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := fs.Stat(ctx, "/"); err != nil {
				t.Errorf("stat: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := r.refreshes.Load(); n != 1 {
		t.Errorf("20 concurrent 401s caused %d refreshes, want 1", n)
	}
}

func TestNoRetryWithoutRefreshToken(t *testing.T) {
	r := newRig(t, "") // bearer token with no refresh token
	fs := r.open(t, r.goodRefresh)
	_, err := fs.Stat(context.Background(), "/")
	if !errors.Is(err, odraz.ErrAuth) || r.refreshes.Load() != 0 || r.denied.Load() != 1 {
		t.Errorf("err=%v refreshes=%d denied=%d", err, r.refreshes.Load(), r.denied.Load())
	}
}

func TestRefreshFailureIsAuthError(t *testing.T) {
	r := newRig(t, "rt")
	fs := r.open(t, func(context.Context, accounts.Credentials) (accounts.Credentials, error) {
		r.refreshes.Add(1)
		return accounts.Credentials{}, errors.New("invalid_grant")
	})
	_, err := fs.Stat(context.Background(), "/")
	if !errors.Is(err, odraz.ErrAuth) || r.refreshes.Load() != 1 {
		t.Errorf("err=%v refreshes=%d", err, r.refreshes.Load())
	}
}

func TestRetriesOnlyOnce(t *testing.T) {
	// A server that rejects even the refreshed token must not cause a loop.
	r := newRig(t, "rt")
	fs := r.open(t, func(context.Context, accounts.Credentials) (accounts.Credentials, error) {
		r.refreshes.Add(1)
		return accounts.Credentials{Kind: accounts.CredBearer, AccessToken: "also-wrong", Expiry: time.Now().Add(time.Hour)}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // a retry loop would hang
	defer cancel()
	if _, err := fs.Stat(ctx, "/"); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("err = %v", err)
	}
	if r.refreshes.Load() != 1 || r.denied.Load() != 2 {
		t.Errorf("refreshes=%d denied=%d, want 1 and 2", r.refreshes.Load(), r.denied.Load())
	}
}

func TestBasicAuthNeverRetries(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(401) }))
	defer srv.Close()
	ctx := context.Background()
	st := accounts.NewMemoryStore()
	st.Put(ctx, "a", accounts.SecretUsername, []byte("u"))
	st.Put(ctx, "a", accounts.SecretPassword, []byte("p"))
	var refreshed atomic.Int32
	h, _ := accounts.Open(ctx, accounts.Account{ID: "a", Adapter: "webdav", Config: map[string]string{"url": srv.URL}}, st,
		accounts.WithRefresh(func(context.Context, accounts.Credentials) (accounts.Credentials, error) {
			refreshed.Add(1)
			return accounts.Credentials{}, nil
		}))
	cl, _ := files.ForAccount(ctx, h)
	fs, _ := cl.OpenDrive(ctx, "default")
	if _, err := fs.Stat(ctx, "/"); !errors.Is(err, odraz.ErrAuth) || hits.Load() != 1 || refreshed.Load() != 0 {
		t.Errorf("err=%v hits=%d refreshed=%d", err, hits.Load(), refreshed.Load())
	}
}

func TestStreamingUploadAfterRevocation(t *testing.T) {
	// The upload body cannot be replayed, so the first attempt fails with
	// ErrAuth, but the token is marked rejected: the next attempt succeeds.
	ctx := context.Background()
	r := newRig(t, "rt")
	fs := r.open(t, r.goodRefresh)
	err := fs.Write(ctx, "/a.txt", strings.NewReader("hello"), files.WriteOptions{Size: 5})
	if !errors.Is(err, odraz.ErrAuth) {
		// Stat inside Write may already have recovered the token; both outcomes are fine
		// as long as a retry works.
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err := fs.Write(ctx, "/a.txt", strings.NewReader("hello"), files.WriteOptions{Size: 5}); err != nil {
		t.Errorf("retry after refresh failed: %v", err)
	}
}
