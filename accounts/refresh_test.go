package accounts

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
)

func seedBearer(t *testing.T, s SecretStore, exp time.Time) {
	t.Helper()
	ctx := context.Background()
	s.Put(ctx, "a", SecretAccessToken, []byte("old"))
	s.Put(ctx, "a", SecretRefreshToken, []byte("r1"))
	s.Put(ctx, "a", SecretExpiry, []byte(exp.UTC().Format(time.RFC3339)))
}

func TestRefreshOnceUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	seedBearer(t, s, time.Now().Add(-time.Minute))
	var calls atomic.Int32
	p := newRefreshingProvider(storeProvider{id: "a", store: s}, func(_ context.Context, old Credentials) (Credentials, error) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond) // widen the race window
		if old.RefreshToken != "r1" {
			t.Errorf("refresh got token %q", old.RefreshToken)
		}
		return Credentials{Kind: CredBearer, AccessToken: "new", Expiry: time.Now().Add(time.Hour)}, nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := p.Credentials(ctx)
			if err != nil || c.AccessToken != "new" {
				t.Errorf("got %+v, %v", c, err)
			}
		}()
	}
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("refresh called %d times, want 1", n)
	}
	// Persisted, with the old refresh token kept because the hook omitted it.
	if v, _ := s.Get(ctx, "a", SecretAccessToken); string(v) != "new" {
		t.Error("access token not saved")
	}
	if v, _ := s.Get(ctx, "a", SecretRefreshToken); string(v) != "r1" {
		t.Error("refresh token must be kept")
	}
	if _, err := p.Credentials(ctx); err != nil || calls.Load() != 1 {
		t.Error("fresh credentials must not trigger another refresh")
	}
}

func TestNoRefreshWhenValidOrNoExpiry(t *testing.T) {
	ctx := context.Background()
	for _, exp := range []time.Time{time.Now().Add(time.Hour), {}} {
		s := NewMemoryStore()
		seedBearer(t, s, exp)
		if exp.IsZero() {
			s.Put(ctx, "a", SecretExpiry, []byte(""))
		}
		p := newRefreshingProvider(storeProvider{id: "a", store: s}, func(context.Context, Credentials) (Credentials, error) {
			t.Error("unexpected refresh")
			return Credentials{}, nil
		})
		if c, err := p.Credentials(ctx); err != nil || c.AccessToken != "old" {
			t.Errorf("%+v %v", c, err)
		}
	}
}

func TestRefreshFailureIsErrAuthAndRetried(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	seedBearer(t, s, time.Now().Add(-time.Minute))
	var calls int
	boom := errors.New("idp down")
	p := newRefreshingProvider(storeProvider{id: "a", store: s}, func(context.Context, Credentials) (Credentials, error) {
		calls++
		if calls == 1 {
			return Credentials{}, boom
		}
		return Credentials{Kind: CredBearer, AccessToken: "ok", Expiry: time.Now().Add(time.Hour)}, nil
	})
	_, err := p.Credentials(ctx)
	if !errors.Is(err, odraz.ErrAuth) || !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if c, err := p.Credentials(ctx); err != nil || c.AccessToken != "ok" {
		t.Fatalf("retry: %+v %v", c, err)
	}
}

func TestExpiredWithoutRefreshToken(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	s.Put(ctx, "a", SecretAccessToken, []byte("t"))
	s.Put(ctx, "a", SecretExpiry, []byte(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)))
	p := newRefreshingProvider(storeProvider{id: "a", store: s}, func(context.Context, Credentials) (Credentials, error) {
		t.Error("must not call refresh without a refresh token")
		return Credentials{}, nil
	})
	if _, err := p.Credentials(ctx); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("err = %v", err)
	}
}

func TestWaiterHonoursContext(t *testing.T) {
	s := NewMemoryStore()
	seedBearer(t, s, time.Now().Add(-time.Minute))
	release := make(chan struct{})
	p := newRefreshingProvider(storeProvider{id: "a", store: s}, func(context.Context, Credentials) (Credentials, error) {
		<-release
		return Credentials{Kind: CredBearer, AccessToken: "n", Expiry: time.Now().Add(time.Hour)}, nil
	})
	go p.Credentials(context.Background())
	time.Sleep(30 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := p.Credentials(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiter err = %v", err)
	}
	close(release)
}

func TestOpenWiresRefresh(t *testing.T) {
	ctx := context.Background()
	Register(Adapter{Name: "refresh-test-adapter"})
	s := NewMemoryStore()
	seedBearer(t, s, time.Now().Add(-time.Minute))
	h, err := Open(ctx, Account{ID: "a", Adapter: "refresh-test-adapter"}, s,
		WithRefresh(func(context.Context, Credentials) (Credentials, error) {
			return Credentials{Kind: CredBearer, AccessToken: "via-option", Expiry: time.Now().Add(time.Hour)}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	if c, err := h.Env.Creds.Credentials(ctx); err != nil || c.AccessToken != "via-option" {
		t.Errorf("%+v %v", c, err)
	}
}
