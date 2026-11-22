package accounts

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
)

func TestMemoryStore(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	if _, err := s.Get(ctx, "a", "k"); !errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	s.Put(ctx, "a", "k", []byte("v"))
	s.Put(ctx, "a", "k2", []byte("v2"))
	if v, _ := s.Get(ctx, "a", "k"); string(v) != "v" {
		t.Fatal("bad value")
	}
	s.Delete(ctx, "a")
	if _, err := s.Get(ctx, "a", "k2"); !errors.Is(err, odraz.ErrNotFound) {
		t.Fatal("delete should remove all keys")
	}
}

func TestExpired(t *testing.T) {
	now := time.Now()
	if (Credentials{}).Expired(now, time.Minute) {
		t.Error("zero expiry never expires")
	}
	c := Credentials{Expiry: now.Add(30 * time.Second)}
	if !c.Expired(now, time.Minute) || c.Expired(now, 0) {
		t.Error("skew handling wrong")
	}
}

func TestEnvProvider(t *testing.T) {
	ctx := context.Background()
	p := EnvProvider{Prefix: "ODRAZTEST"}
	if _, err := p.Credentials(ctx); !errors.Is(err, odraz.ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
	t.Setenv("ODRAZTEST_USERNAME", "u")
	t.Setenv("ODRAZTEST_PASSWORD", "p")
	if c, _ := p.Credentials(ctx); c.Kind != CredBasic || c.Username != "u" || c.Password != "p" {
		t.Fatalf("got %+v", c)
	}
	t.Setenv("ODRAZTEST_TOKEN", "t")
	if c, _ := p.Credentials(ctx); c.Kind != CredBearer || c.AccessToken != "t" {
		t.Fatalf("got %+v", c)
	}
}

func TestAvatarCapability(t *testing.T) {
	ctx := context.Background()
	var gotSize int
	Register(Adapter{Name: "avatar-yes", Avatar: func(_ context.Context, _ *Env, size int) (io.ReadCloser, string, error) {
		gotSize = size
		return io.NopCloser(strings.NewReader("img")), "image/png", nil
	}})
	Register(Adapter{Name: "avatar-no"})
	open := func(name string) *Handle {
		h, err := Open(ctx, Account{ID: "x", Adapter: name}, NewMemoryStore())
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if _, _, err := open("avatar-no").Avatar(ctx, 64); !errors.Is(err, odraz.ErrUnsupported) {
		t.Errorf("adapter without avatars: %v, want ErrUnsupported", err)
	}
	h := open("avatar-yes")
	for _, c := range []struct{ in, want int }{{0, 64}, {1, 1}, {128, 128}, {MaxAvatarSize, MaxAvatarSize}} {
		rc, ct, err := h.Avatar(ctx, c.in)
		if err != nil || ct != "image/png" || gotSize != c.want {
			t.Errorf("Avatar(%d) = %v %q, adapter saw size %d, want %d", c.in, err, ct, gotSize, c.want)
			continue
		}
		rc.Close()
	}
	for _, bad := range []int{-1, MaxAvatarSize + 1} {
		if _, _, err := h.Avatar(ctx, bad); !errors.Is(err, odraz.ErrInvalidPath) {
			t.Errorf("Avatar(%d) = %v, want ErrInvalidPath", bad, err)
		}
	}
}
