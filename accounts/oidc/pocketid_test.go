package oidc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts/oidc"
)

// Real external provider: Pocket ID (passkey-only login). The test skips the
// passkey by using an admin API key to create a user and a one-time access
// token, which an ordinary browser session can use instead of a passkey.
//
//	eval "$(testenv/pocketid/seed.sh)" && go test ./accounts/oidc -run PocketID

type pocketID struct {
	t      *testing.T
	url    string
	apiKey string
	hc     *http.Client
}

func newPocketID(t *testing.T) *pocketID {
	u, k := os.Getenv("ODRAZ_TEST_POCKETID_URL"), os.Getenv("ODRAZ_TEST_POCKETID_API_KEY")
	if u == "" || k == "" {
		t.Skip("ODRAZ_TEST_POCKETID_* not set (testenv/pocketid/seed.sh)")
	}
	return &pocketID{t: t, url: u, apiKey: k, hc: &http.Client{Timeout: 20 * time.Second}}
}

func (p *pocketID) admin(method, path string, body, out any) {
	p.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, p.url+path, r)
	req.Header.Set("X-API-KEY", p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		p.t.Fatalf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			p.t.Fatalf("%s %s: %v: %s", method, path, err, raw)
		}
	}
}

type fixture struct{ username, userID, clientID string }

// setup creates a fresh user and a public PKCE client that allows loopback redirects.
func (p *pocketID) setup() fixture {
	n := rand.Int63()
	f := fixture{username: fmt.Sprintf("alice%d", n), clientID: fmt.Sprintf("odraz-%d", n)}
	var u struct{ ID string }
	p.admin("POST", "/api/users", map[string]any{"username": f.username, "email": f.username + "@example.test", "emailVerified": true,
		"firstName": "Alice", "lastName": "Test", "displayName": "Alice Test"}, &u)
	f.userID = u.ID
	p.admin("POST", "/api/oidc/clients", map[string]any{"id": f.clientID, "name": "Odraz SDK test",
		"callbackURLs": []string{"http://127.0.0.1/callback", "http://localhost/callback"}, "isPublic": true, "pkceEnabled": true}, nil)
	p.t.Cleanup(func() {
		p.admin("DELETE", "/api/oidc/clients/"+f.clientID, nil, nil)
		p.admin("DELETE", "/api/users/"+f.userID, nil, nil)
	})
	return f
}

// browser is a logged-in browser session (via one-time access token).
func (p *pocketID) browser(userID string) *http.Client {
	var tok struct{ Token string }
	p.admin("POST", "/api/users/"+userID+"/one-time-access-token", map[string]any{"ttl": "15m"}, &tok)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Post(p.url+"/api/one-time-access-token/"+tok.Token, "application/json", strings.NewReader("{}"))
	if err != nil || resp.StatusCode != 200 {
		p.t.Fatalf("exchanging the one-time access token: %v %v", resp, err)
	}
	resp.Body.Close()
	return c
}

// authorize plays the browser through /authorize, completing the consent
// interaction if asked, and finally follows the redirect to the app (the
// loopback listener). Returns the HTTP status of the final hop.
func (p *pocketID) authorize(b *http.Client, authURL string) error {
	next := authURL
	for hop := 0; hop < 6; hop++ {
		resp, err := b.Get(next)
		if err != nil {
			return err
		}
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if loc == "" {
			return fmt.Errorf("authorize answered HTTP %d without a redirect", resp.StatusCode)
		}
		lu, _ := url.Parse(loc)
		abs := resp.Request.URL.ResolveReference(lu)
		switch {
		case strings.HasPrefix(abs.Path, "/interaction/error"):
			return fmt.Errorf("provider refused: %s", abs.Query().Get("error"))
		case abs.Path == "/interaction":
			id := abs.Query().Get("interaction")
			var it struct {
				Scopes        []string `json:"scopes"`
				RequiredSteps []string `json:"requiredSteps"`
			}
			if err := p.getJSON(b, "/api/oidc/interactions/"+id, &it); err != nil {
				return err
			}
			for _, step := range it.RequiredSteps {
				body, _ := json.Marshal(map[string]any{"step": step, "scopes": it.Scopes})
				r, err := b.Post(p.url+"/api/oidc/interactions/"+id+"/complete", "application/json", bytes.NewReader(body))
				if err != nil {
					return err
				}
				r.Body.Close()
				if r.StatusCode != 200 {
					return fmt.Errorf("completing %q: HTTP %d", step, r.StatusCode)
				}
			}
			next = p.url + "/authorize?interaction=" + id
		case strings.HasPrefix(abs.String(), p.url):
			next = abs.String()
		default: // the app's redirect URI: deliver it like a browser would
			r, err := p.hc.Get(abs.String())
			if err != nil {
				return err
			}
			r.Body.Close()
			return nil
		}
	}
	return errors.New("too many redirects in the authorize flow")
}

func (p *pocketID) getJSON(b *http.Client, path string, out any) error {
	resp, err := b.Get(p.url + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, raw)
	}
	return json.Unmarshal(raw, out)
}

func TestPocketIDLoginRefreshAndUserinfo(t *testing.T) {
	p := newPocketID(t)
	f := p.setup()
	username, clientID := f.username, f.clientID
	b := p.browser(f.userID)

	// Discovery: Pocket ID has no WebFinger, so the server URL is the issuer.
	prov, err := oidc.Discover(ctx, nil, p.url)
	if err != nil || prov.TokenEndpoint != p.url+"/api/oidc/token" {
		t.Fatalf("discover: %+v %v", prov, err)
	}

	// The desktop flow: loopback listener on a random port.
	lb, err := oidc.NewLoopback()
	if err != nil {
		t.Fatal(err)
	}
	defer lb.Close()
	sess, err := oidc.Begin(ctx, nil, p.url, oidc.Config{ClientID: clientID, RedirectURI: lb.RedirectURI()})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.authorize(b, sess.AuthorizationURL); err != nil {
		t.Fatalf("browser flow: %v", err)
	}
	wctx, cancel := context_WithTimeout(10 * time.Second)
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
	req, _ := http.NewRequest("GET", p.url+"/api/oidc/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	resp, err := p.hc.Do(req)
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
	refresh := oidc.RefreshFunc(nil, p.url, clientID)
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
	p := newPocketID(t)
	f := p.setup()
	clientID := f.clientID
	b := p.browser(f.userID)

	// A redirect URI the admin did not register is refused by the provider.
	sess, err := oidc.Begin(ctx, nil, p.url, oidc.Config{ClientID: clientID, RedirectURI: "http://127.0.0.1:7777/not-registered"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.authorize(b, sess.AuthorizationURL); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("unregistered redirect: %v", err)
	}
	// An unknown client is refused too.
	sess, _ = oidc.Begin(ctx, nil, p.url, oidc.Config{ClientID: "no-such-client", RedirectURI: "http://127.0.0.1:7777/callback"})
	if err := p.authorize(b, sess.AuthorizationURL); err == nil {
		t.Error("unknown client must be refused")
	}
	// Refreshing with a bogus refresh token is an auth failure.
	if _, err := oidc.RefreshFunc(nil, p.url, clientID)(ctx, accounts.Credentials{RefreshToken: "bogus"}); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("bogus refresh token: %v", err)
	}
}

func context_WithTimeout(d time.Duration) (c context.Context, cancel context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}
