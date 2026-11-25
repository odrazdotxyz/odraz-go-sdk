package s3_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/drivers/s3"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
)

func account(t *testing.T, key, secret string, cfg map[string]string) (accounts.Account, accounts.SecretStore) {
	t.Helper()
	ctx := context.Background()
	store := accounts.NewMemoryStore()
	store.Put(ctx, "acc", accounts.SecretUsername, []byte(key))
	store.Put(ctx, "acc", accounts.SecretPassword, []byte(secret))
	return accounts.Account{ID: "acc", Label: "Test", Adapter: "s3", Config: cfg}, store
}

func TestRegisteredAdapter(t *testing.T) {
	a, ok := accounts.Lookup("s3")
	if !ok {
		t.Fatal("s3 adapter is not registered")
	}
	if _, ok := a.Domains[files.Domain]; !ok {
		t.Fatal("s3 adapter has no files domain")
	}
	if len(a.AuthMethods) != 1 || a.AuthMethods[0].ID != "access-key" || a.AuthMethods[0].Kind != accounts.AuthForm {
		t.Fatalf("auth methods = %+v", a.AuthMethods)
	}
}

func TestValidateOK(t *testing.T) {
	ep := newFakeS3(t)
	acc, store := account(t, fakeAccessKey, fakeSecret, map[string]string{"endpoint": ep, "bucket": fakeBucket})
	if err := accounts.Validate(context.Background(), acc, store); err != nil {
		t.Fatal(err)
	}
}

func TestValidateWrongKeys(t *testing.T) {
	ep := newFakeS3(t)
	const secret = "WRONGSECRETWRONGSECRET0987654321"
	acc, store := account(t, "AKIAWRONG", secret, map[string]string{"endpoint": ep, "bucket": fakeBucket})
	err := accounts.Validate(context.Background(), acc, store)
	if !errors.Is(err, odraz.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	for _, s := range []string{secret, "AKIAWRONG"} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error leaks a credential: %v", err)
		}
	}
}

func TestValidateMissingBucket(t *testing.T) {
	ep := newFakeS3(t)
	acc, store := account(t, fakeAccessKey, fakeSecret, map[string]string{"endpoint": ep, "bucket": "nope"})
	if err := accounts.Validate(context.Background(), acc, store); !errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestValidateUnreachable(t *testing.T) {
	// Port 1 has no listener.
	acc, store := account(t, fakeAccessKey, fakeSecret, map[string]string{"endpoint": "http://127.0.0.1:1", "bucket": fakeBucket})
	err := accounts.Validate(context.Background(), acc, store)
	if err == nil || errors.Is(err, odraz.ErrAuth) || errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("err = %v, want a connection error", err)
	}
}

func TestDrivesSingleDrive(t *testing.T) {
	ep := newFakeS3(t)
	acc, store := account(t, fakeAccessKey, fakeSecret, map[string]string{"endpoint": ep, "bucket": fakeBucket})
	ctx := context.Background()
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
	d := drives[0]
	if d.ID != "default" || d.Name != "Test" || d.Kind != files.DriveOther || d.Quota != nil {
		t.Fatalf("drive = %+v", d)
	}
	if _, err := cl.OpenDrive(ctx, "other"); !errors.Is(err, odraz.ErrNotFound) {
		t.Fatalf("OpenDrive(other) = %v, want ErrNotFound", err)
	}
}

func TestEndpointVariants(t *testing.T) {
	plain := newFakeS3(t)
	tlsEP := newFakeS3TLS(t)
	hostPort := strings.TrimPrefix(tlsEP, "https://")
	cases := []struct {
		name string
		cfg  map[string]string
	}{
		{"http url", map[string]string{"endpoint": plain}},
		{"trailing slash", map[string]string{"endpoint": plain + "/"}},
		{"https url, insecure", map[string]string{"endpoint": tlsEP, "insecure_skip_verify": "true"}},
		{"host:port defaults to https", map[string]string{"endpoint": hostPort, "path_style": "true", "insecure_skip_verify": "true"}},
		{"host:port path style implied", map[string]string{"endpoint": hostPort, "insecure_skip_verify": "true"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.cfg["bucket"] = fakeBucket
			acc, store := account(t, fakeAccessKey, fakeSecret, c.cfg)
			if err := accounts.Validate(context.Background(), acc, store); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEndpointRejectsPath(t *testing.T) {
	acc, store := account(t, fakeAccessKey, fakeSecret, map[string]string{"endpoint": "http://example.com/some/path", "bucket": "b"})
	if err := accounts.Validate(context.Background(), acc, store); err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("err = %v, want a path complaint", err)
	}
}

// Credentials in the endpoint are refused, and no endpoint error repeats them.
func TestEndpointNeverEchoesSecrets(t *testing.T) {
	const secret = "TOPSECRETVALUE"
	for _, ep := range []string{
		"https://AKIAUSER:" + secret + "@example.com",
		"https://AKIAUSER:" + secret + "@example.com/p",
		"http://" + secret + "@example.com",
		"AKIAUSER:" + secret + "@example.com",
		"ftp://AKIAUSER:" + secret + "@example.com",
		"https://AKIAUSER:" + secret + " x@example.com",
		"https://AKIAUSER:" + secret + "/x@example.com",
		"https://AKIAUSER:1234/" + secret + "@example.com",
		"https://AKIAUSER:1234?" + secret + "@example.com",
		"https://AKIAUSER:1234#" + secret + "@example.com",
		"https://example.com/" + secret,
		"https://example.com?key=" + secret,
		"https://example.com#" + secret,
	} {
		acc, store := account(t, fakeAccessKey, fakeSecret, map[string]string{"endpoint": ep, "bucket": "b"})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		verr := accounts.Validate(ctx, acc, store)
		cancel()
		_, nerr := s3.New(&accounts.Env{Account: acc})
		for _, err := range []error{verr, nerr} {
			if err == nil {
				t.Errorf("%q accepted", ep)
				continue
			}
			for _, bad := range []string{secret, "AKIAUSER", "1234"} {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("%q: error leaks %s: %v", ep, bad, err)
				}
			}
			if !strings.Contains(err.Error(), "endpoint") {
				t.Errorf("%q: unclear error: %v", ep, err)
			}
		}
	}
}
