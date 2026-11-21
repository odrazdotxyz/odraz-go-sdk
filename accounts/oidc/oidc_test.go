package oidc_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts/oidc"
)

var ctx = context.Background()

func cfg(f *fakeIdP) oidc.Config { return oidc.Config{ClientID: f.ClientID, RedirectURI: f.Redirect} }

func TestDiscover(t *testing.T) {
	f := newFakeIdP(t)
	p, err := oidc.Discover(ctx, nil, f.URL)
	if err != nil || p.Issuer != f.URL || p.TokenEndpoint != f.URL+"/token" {
		t.Fatalf("%+v %v", p, err)
	}
	// Without WebFinger the server URL itself is the issuer.
	f.Webfinger = false
	if _, err := oidc.Discover(ctx, nil, f.URL+"/"); err != nil {
		t.Errorf("fallback: %v", err)
	}
}

func TestDiscoverIssuerPathAndMismatch(t *testing.T) {
	// Issuer under a path, advertised through WebFinger.
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/.well-known/webfinger", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"links":[{"rel":"http://openid.net/specs/connect/1.0/issuer","href":"`+srv.URL+`/realm/"}]}`)
	})
	mux.HandleFunc("/realm/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"issuer":"`+srv.URL+`/realm","authorization_endpoint":"`+srv.URL+`/a","token_endpoint":"`+srv.URL+`/t"}`)
	})
	p, err := oidc.Discover(ctx, nil, srv.URL)
	if err != nil || p.AuthorizationEndpoint != srv.URL+"/a" {
		t.Fatalf("%+v %v", p, err)
	}
	// A configuration that names another issuer must be refused.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"issuer":"https://evil.example","authorization_endpoint":"x","token_endpoint":"y"}`)
	}))
	defer bad.Close()
	if _, err := oidc.Discover(ctx, nil, bad.URL); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("issuer mismatch: %v", err)
	}
}

func TestDiscoverBehindProxy(t *testing.T) {
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html>Sign in</html>")
	}))
	defer html.Close()
	if _, err := oidc.Discover(ctx, nil, html.URL); !errors.Is(err, odraz.ErrBlockedByProxy) {
		t.Errorf("err = %v", err)
	}
}

func TestLoginFlow(t *testing.T) {
	f := newFakeIdP(t)
	sess, err := oidc.Begin(ctx, nil, f.URL, cfg(f))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(sess.AuthorizationURL)
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" || q.Get("state") == "" ||
		q.Get("scope") != "openid profile email offline_access" || q.Get("client_id") != f.ClientID || q.Get("code_challenge") == "" {
		t.Fatalf("authorization URL = %s", sess.AuthorizationURL)
	}
	if strings.Contains(sess.AuthorizationURL, sess.Verifier) {
		t.Error("the PKCE verifier must never be in the URL")
	}
	toks, err := sess.Complete(ctx, browse(t, sess.AuthorizationURL))
	if err != nil {
		t.Fatal(err)
	}
	if toks.AccessToken == "" || toks.RefreshToken == "" || time.Until(toks.Expiry) < 59*time.Minute {
		t.Errorf("tokens = %+v", toks)
	}
	// Tokens land where accounts.Open reads them.
	store := accounts.NewMemoryStore()
	if err := oidc.Save(ctx, store, "a", toks); err != nil {
		t.Fatal(err)
	}
	accounts.Register(accounts.Adapter{Name: "oidc-save-test"})
	h, err := accounts.Open(ctx, accounts.Account{ID: "a", Adapter: "oidc-save-test"}, store)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := h.Env.Creds.Credentials(ctx); c.Kind != accounts.CredBearer || c.AccessToken != toks.AccessToken || c.RefreshToken != toks.RefreshToken {
		t.Errorf("creds = %+v", c)
	}
}

func TestLoginRejections(t *testing.T) {
	f := newFakeIdP(t)
	sess, _ := oidc.Begin(ctx, nil, f.URL, cfg(f))
	cb := browse(t, sess.AuthorizationURL)
	cu, _ := url.Parse(cb)

	// Wrong state: a response from some other login attempt.
	bad := *cu
	b := bad.Query()
	b.Set("state", "someone-elses")
	bad.RawQuery = b.Encode()
	if _, err := sess.Complete(ctx, bad.String()); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("state mismatch: %v", err)
	}
	// The user denied access.
	if _, err := sess.Complete(ctx, f.Redirect+"?error=access_denied&error_description=no&state="+sess.State); !errors.Is(err, odraz.ErrAuth) || !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("denied: %v", err)
	}
	// No code.
	if _, err := sess.Complete(ctx, f.Redirect+"?state="+sess.State); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("no code: %v", err)
	}
	// A code is single use.
	if _, err := sess.Complete(ctx, cb); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Complete(ctx, cb); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("code replay: %v", err)
	}
	// PKCE: a code obtained with a different verifier is useless.
	s1, _ := oidc.Begin(ctx, nil, f.URL, cfg(f))
	s2, _ := oidc.Begin(ctx, nil, f.URL, cfg(f))
	cb1 := browse(t, s1.AuthorizationURL)
	cu1, _ := url.Parse(cb1)
	q1 := cu1.Query()
	q1.Set("state", s2.State) // s2 accepts the callback state, but the code belongs to s1's verifier
	cu1.RawQuery = q1.Encode()
	if _, err := s2.Complete(ctx, cu1.String()); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("PKCE must reject a foreign verifier: %v", err)
	}
	// Unregistered client: the IdP refuses before any code exists.
	if _, err := oidc.Begin(ctx, nil, f.URL, oidc.Config{ClientID: "", RedirectURI: f.Redirect}); err == nil {
		t.Error("empty client id must be rejected")
	}
}

func TestSessionSurvivesRestart(t *testing.T) {
	f := newFakeIdP(t)
	sess, _ := oidc.Begin(ctx, nil, f.URL, cfg(f))
	saved, err := sess.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	cb := browse(t, sess.AuthorizationURL)
	restored, err := oidc.Resume(saved, nil) // the app was killed while the user was in the browser
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restored.Complete(ctx, cb); err != nil {
		t.Errorf("resumed session: %v", err)
	}
	if _, err := oidc.Resume([]byte(`{}`), nil); err == nil {
		t.Error("garbage must not resume")
	}
}

func TestRefreshFunc(t *testing.T) {
	f := newFakeIdP(t)
	sess, _ := oidc.Begin(ctx, nil, f.URL, cfg(f))
	toks, _ := sess.Complete(ctx, browse(t, sess.AuthorizationURL))
	refresh := oidc.RefreshFunc(nil, f.URL, f.ClientID)
	nc, err := refresh(ctx, accounts.Credentials{Kind: accounts.CredBearer, AccessToken: toks.AccessToken, RefreshToken: toks.RefreshToken})
	if err != nil || nc.AccessToken == toks.AccessToken || nc.RefreshToken == toks.RefreshToken || nc.Kind != accounts.CredBearer {
		t.Fatalf("%+v %v", nc, err)
	}
	// The old refresh token was rotated away: replaying it is an auth failure.
	if _, err := refresh(ctx, accounts.Credentials{RefreshToken: toks.RefreshToken}); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("replayed refresh token: %v", err)
	}
	// A provider that omits the refresh token keeps the old one.
	t.Run("keeps old refresh token when omitted", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/.well-known/openid-configuration":
				io.WriteString(w, `{"issuer":"http://`+r.Host+`","authorization_endpoint":"x","token_endpoint":"http://`+r.Host+`/t"}`)
			case "/t":
				io.WriteString(w, `{"access_token":"new","expires_in":60}`)
			default:
				w.WriteHeader(404)
			}
		}))
		defer srv.Close()
		nc, err := oidc.RefreshFunc(nil, srv.URL, "c")(ctx, accounts.Credentials{RefreshToken: "keep-me"})
		if err != nil || nc.RefreshToken != "keep-me" || nc.AccessToken != "new" {
			t.Errorf("%+v %v", nc, err)
		}
	})
}

func TestLoopback(t *testing.T) {
	lb, err := oidc.NewLoopback()
	if err != nil {
		t.Fatal(err)
	}
	defer lb.Close()
	if !strings.HasPrefix(lb.RedirectURI(), "http://127.0.0.1:") || !strings.HasSuffix(lb.RedirectURI(), "/callback") {
		t.Fatalf("redirect = %s", lb.RedirectURI())
	}
	go func() { // the browser arrives
		time.Sleep(20 * time.Millisecond)
		http.Get(lb.RedirectURI() + "?code=abc&state=xyz")
	}()
	got, err := lb.Wait(ctx)
	if err != nil || got != lb.RedirectURI()+"?code=abc&state=xyz" {
		t.Errorf("got %q, %v", got, err)
	}
	c, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	lb2, _ := oidc.NewLoopback()
	defer lb2.Close()
	if _, err := lb2.Wait(c); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("wait must honour ctx: %v", err)
	}
}

func TestEndSessionURL(t *testing.T) {
	p := oidc.Provider{EndSessionEndpoint: "https://idp/logout"}
	u := p.EndSessionURL("tok", "http://127.0.0.1/bye")
	if !strings.Contains(u, "id_token_hint=tok") || !strings.Contains(u, "post_logout_redirect_uri=") {
		t.Error(u)
	}
	if (&oidc.Provider{}).EndSessionURL("", "") != "" {
		t.Error("no endpoint must give empty URL")
	}
}
