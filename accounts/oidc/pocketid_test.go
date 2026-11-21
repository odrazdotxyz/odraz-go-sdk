package oidc_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts/oidc"
	"github.com/odrazdotxyz/odraz-go-sdk/internal/idptest"
)

// Real external provider: Pocket ID (passkey-only login). See internal/idptest and testenv/pocketid.
//
//	eval "$(testenv/pocketid/seed.sh)" && go test ./accounts/oidc -run PocketID

func TestPocketIDLoginRefreshAndUserinfo(t *testing.T) {
	p := idptest.New(t)
	f := p.Setup()
	username, clientID := f.Username, f.ClientID
	b := p.Browser(f.UserID)

	// Discovery: Pocket ID has no WebFinger, so the server URL is the issuer.
	prov, err := oidc.Discover(ctx, nil, p.URL)
	if err != nil || prov.TokenEndpoint != p.URL+"/api/oidc/token" {
		t.Fatalf("discover: %+v %v", prov, err)
	}

	// The desktop flow: loopback listener on a random port.
	lb, err := oidc.NewLoopback()
	if err != nil {
		t.Fatal(err)
	}
	defer lb.Close()
	sess, err := oidc.Begin(ctx, nil, p.URL, oidc.Config{ClientID: clientID, RedirectURI: lb.RedirectURI()})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Authorize(b, sess.AuthorizationURL); err != nil {
		t.Fatalf("browser flow: %v", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	callback, err := lb.Wait(wctx)
	if err != nil {
		t.Fatalf("the loopback listener never received the redirect: %v", err)
	}
	tokens, err := sess.Complete(ctx, callback)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.Expiry.IsZero() {
		t.Fatalf("tokens = %+v (a refresh token needs offline_access)", tokens)
	}

	// The access token is valid at the provider's userinfo endpoint.
	req, _ := http.NewRequest("GET", p.URL+"/api/oidc/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	resp, err := p.HC.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var ui map[string]any
	json.NewDecoder(resp.Body).Decode(&ui)
	resp.Body.Close()
	if resp.StatusCode != 200 || ui["preferred_username"] != username {
		t.Errorf("userinfo: HTTP %d %v (want preferred_username %q)", resp.StatusCode, ui, username)
	}

	// Refresh through the SDK, and what the provider does with the old refresh token.
	refresh := oidc.RefreshFunc(nil, p.URL, clientID)
	nc, err := refresh(ctx, accounts.Credentials{Kind: accounts.CredBearer, RefreshToken: tokens.RefreshToken})
	if err != nil || nc.AccessToken == "" || nc.AccessToken == tokens.AccessToken {
		t.Fatalf("refresh: %+v %v", nc, err)
	}
	if nc.RefreshToken == tokens.RefreshToken {
		t.Log("observed: Pocket ID did not rotate the refresh token")
	} else {
		_, err := refresh(ctx, accounts.Credentials{RefreshToken: tokens.RefreshToken})
		t.Logf("observed: Pocket ID rotates refresh tokens; replaying the old one -> %v (ErrAuth=%v)", err, errors.Is(err, odraz.ErrAuth))
	}

	// A code works once only.
	if _, err := sess.Complete(ctx, callback); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("code replay: %v", err)
	}
}

func TestPocketIDRefusals(t *testing.T) {
	p := idptest.New(t)
	f := p.Setup()
	clientID := f.ClientID
	b := p.Browser(f.UserID)

	// A redirect URI the admin did not register is refused by the provider.
	sess, err := oidc.Begin(ctx, nil, p.URL, oidc.Config{ClientID: clientID, RedirectURI: "http://127.0.0.1:7777/not-registered"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Authorize(b, sess.AuthorizationURL); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("unregistered redirect: %v", err)
	}
	// An unknown client is refused too.
	sess, _ = oidc.Begin(ctx, nil, p.URL, oidc.Config{ClientID: "no-such-client", RedirectURI: "http://127.0.0.1:7777/callback"})
	if err := p.Authorize(b, sess.AuthorizationURL); err == nil {
		t.Error("unknown client must be refused")
	}
	// Refreshing with a bogus refresh token is an auth failure.
	if _, err := oidc.RefreshFunc(nil, p.URL, clientID)(ctx, accounts.Credentials{RefreshToken: "bogus"}); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("bogus refresh token: %v", err)
	}
}
