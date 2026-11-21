package opencloud_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts/oidc"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
	"github.com/odrazdotxyz/odraz-go-sdk/internal/idptest"
)

// The whole chain: OpenCloud configured to trust Pocket ID as its external OIDC
// provider (testenv/opencloud-pocketid). The SDK is given only the OpenCloud URL:
// WebFinger on OpenCloud names Pocket ID as the issuer, the login happens at Pocket ID,
// and the resulting token is used on OpenCloud's Graph API and WebDAV.
//
//	testenv/opencloud-pocketid/seed.sh, then:
//	ODRAZ_TEST_OCPOCKET_URL=https://localhost:9200 ODRAZ_TEST_POCKETID_URL=... go test -run OpenCloudWithPocketID
func TestOpenCloudWithPocketID(t *testing.T) {
	ocURL := os.Getenv("ODRAZ_TEST_OCPOCKET_URL")
	if ocURL == "" {
		t.Skip("ODRAZ_TEST_OCPOCKET_URL not set (testenv/opencloud-pocketid)")
	}
	pid := idptest.New(t)
	f := pid.Setup()
	browser := pid.Browser(f.UserID)

	acc := accounts.Account{ID: "oc", Adapter: "opencloud", Config: map[string]string{
		"url": ocURL, accounts.ConfigInsecure: "true", accounts.ConfigOIDCClientID: f.ClientID}}
	store := accounts.NewMemoryStore()
	h0, err := accounts.Open(ctx, acc, store)
	if err != nil {
		t.Fatal(err)
	}
	hc := oidc.HTTPClient(h0.Env)

	// Discovery through OpenCloud lands on Pocket ID.
	prov, err := oidc.Discover(ctx, hc, ocURL)
	if err != nil || !strings.HasPrefix(prov.AuthorizationEndpoint, pid.URL) {
		t.Fatalf("discovery via OpenCloud should name Pocket ID: %+v %v", prov, err)
	}

	lb, err := oidc.NewLoopback()
	if err != nil {
		t.Fatal(err)
	}
	defer lb.Close()
	sess, err := oidc.Begin(ctx, hc, ocURL, oidc.Config{ClientID: f.ClientID, RedirectURI: lb.RedirectURI(), Scopes: []string{"openid", "profile", "email", "offline_access"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := pid.Authorize(browser, sess.AuthorizationURL); err != nil {
		t.Fatalf("browser flow: %v", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	callback, err := lb.Wait(wctx)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := sess.Complete(ctx, callback)
	if err != nil {
		t.Fatal(err)
	}
	if err := oidc.Save(ctx, store, "oc", tokens); err != nil {
		t.Fatal(err)
	}

	open := func() files.Client {
		h, err := accounts.Open(ctx, acc, store)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := files.ForAccount(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		return cl
	}

	// OpenCloud accepts Pocket ID's token (and auto-provisions the user).
	drives, err := open().Drives(ctx)
	if err != nil {
		t.Fatalf("OpenCloud rejected the Pocket ID token: %v", err)
	}
	var personal string
	for _, d := range drives {
		t.Logf("drive: %s %q", d.Kind, d.Name)
		if d.Kind == files.DrivePersonal {
			personal = d.ID
		}
	}
	if personal == "" {
		t.Fatalf("no personal drive for the OIDC user: %v", drives)
	}
	fs, err := open().OpenDrive(ctx, personal)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Upload(ctx, fs, "/hello/oidc.txt", strings.NewReader("hi"), files.WriteOptions{Size: 2, Overwrite: true}); err != nil {
		t.Fatalf("upload with the Pocket ID token: %v", err)
	}
	// Seen once, right after OpenCloud's cold start: the first upload into the first
	// freshly provisioned space was not immediately visible to Stat. Wait a bounded time
	// and say so, instead of hiding it or failing on a server warm-up effect.
	var st files.Entry
	for attempt := 1; attempt <= 100; attempt++ {
		if st, err = fs.Stat(ctx, "/hello/oidc.txt"); err == nil {
			if attempt > 1 {
				t.Logf("NOTE: the uploaded file became visible to Stat only on attempt %d (server warm-up)", attempt)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || st.Size != 2 {
		t.Fatalf("stat: %+v %v", st, err)
	}

	// Refresh at Pocket ID before the token expires; OpenCloud accepts the new one.
	old, _ := store.Get(ctx, "oc", accounts.SecretAccessToken)
	store.Put(ctx, "oc", accounts.SecretExpiry, []byte(time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)))
	if _, err := open().Drives(ctx); err != nil {
		t.Fatalf("after refresh: %v", err)
	}
	if now, _ := store.Get(ctx, "oc", accounts.SecretAccessToken); string(now) == string(old) {
		t.Error("access token was not refreshed")
	}

	// A token OpenCloud rejects (401) is recovered through the refresh token.
	store.Put(ctx, "oc", accounts.SecretAccessToken, []byte("not-a-real-token"))
	store.Put(ctx, "oc", accounts.SecretExpiry, []byte(time.Now().Add(time.Hour).UTC().Format(time.RFC3339)))
	if _, err := open().Drives(ctx); err != nil {
		t.Fatalf("recovery from a rejected token: %v", err)
	}

	// Without a refresh token a rejected token is just an auth error.
	store.Put(ctx, "oc", accounts.SecretAccessToken, []byte("not-a-real-token"))
	store.Put(ctx, "oc", accounts.SecretRefreshToken, []byte(""))
	if _, err := open().Drives(ctx); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("rejected token without refresh token: %v", err)
	}
}

// Regression: a user that already exists in OpenCloud (here its built-in "admin", with an email) logs in
// again through Pocket ID using the SDK's DEFAULT scopes. OpenCloud's autoprovisioning compares
// the token claims with the stored user on every login; without an email claim it tried to blank the email
// and answered 500 from the second login on. The default scopes must include "email".
func TestOpenCloudWithPocketIDExistingUser(t *testing.T) {
	ocURL := os.Getenv("ODRAZ_TEST_OCPOCKET_URL")
	if ocURL == "" {
		t.Skip("ODRAZ_TEST_OCPOCKET_URL not set (testenv/opencloud-pocketid)")
	}
	pid := idptest.New(t)
	userID := pid.EnsureUser("admin", "admin@pocketid.test") // same name as OpenCloud's admin: mapped by preferred_username
	pid.EnsureClient("odraz-existing-user")

	acc := accounts.Account{ID: "oc", Adapter: "opencloud", Config: map[string]string{
		"url": ocURL, accounts.ConfigInsecure: "true", accounts.ConfigOIDCClientID: "odraz-existing-user"}}
	for login := 1; login <= 2; login++ {
		if login > 1 {
			time.Sleep(15 * time.Second) // let OpenCloud's cached user expire, so the claims are compared again
		}
		store := accounts.NewMemoryStore()
		h0, err := accounts.Open(ctx, acc, store)
		if err != nil {
			t.Fatal(err)
		}
		lb, err := oidc.NewLoopback()
		if err != nil {
			t.Fatal(err)
		}
		sess, err := oidc.Begin(ctx, oidc.HTTPClient(h0.Env), ocURL, oidc.Config{ClientID: "odraz-existing-user", RedirectURI: lb.RedirectURI()}) // default scopes
		if err != nil {
			lb.Close()
			t.Fatal(err)
		}
		if err := pid.Authorize(pid.Browser(userID), sess.AuthorizationURL); err != nil {
			lb.Close()
			t.Fatalf("login %d: browser flow: %v", login, err)
		}
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		callback, err := lb.Wait(wctx)
		cancel()
		lb.Close()
		if err != nil {
			t.Fatal(err)
		}
		tokens, err := sess.Complete(ctx, callback)
		if err != nil {
			t.Fatalf("login %d: %v", login, err)
		}
		if err := oidc.Save(ctx, store, "oc", tokens); err != nil {
			t.Fatal(err)
		}
		h, err := accounts.Open(ctx, acc, store)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := files.ForAccount(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cl.Drives(ctx); err != nil {
			t.Fatalf("login %d: OpenCloud failed for an existing user (scopes %v): %v", login, sess.AuthorizationURL, err)
		}
	}
}
