package accounts

import (
	"context"
	"errors"
	"testing"
	"time"

	odraz "github.com/odraz/sdk"
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
