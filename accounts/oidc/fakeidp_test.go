package oidc_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// fakeIdP is a minimal OIDC provider: discovery, authorize, token (code with
// PKCE, refresh with rotation).
type fakeIdP struct {
	*httptest.Server
	mu         sync.Mutex
	codes      map[string]codeInfo // code -> challenge etc.
	refresh    map[string]bool     // valid refresh tokens
	n          int
	Webfinger  bool   // serve /.well-known/webfinger
	IssuerPath string // optional path under the server for the issuer
	LastAuth   url.Values
	Refreshes  int
	AccessTTL  int
	ClientID   string
	Redirect   string
}

type codeInfo struct{ challenge, redirect, client string }

func newFakeIdP(t *testing.T) *fakeIdP {
	f := &fakeIdP{codes: map[string]codeInfo{}, refresh: map[string]bool{}, Webfinger: true, AccessTTL: 3600, ClientID: "odraz-test", Redirect: "http://127.0.0.1:9999/callback"}
	mux := http.NewServeMux()
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	issuer := func() string { return f.URL + f.IssuerPath }
	mux.HandleFunc("/.well-known/webfinger", func(w http.ResponseWriter, r *http.Request) {
		if !f.Webfinger {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"subject": r.URL.Query().Get("resource"),
			"links": []map[string]string{{"rel": "http://openid.net/specs/connect/1.0/issuer", "href": issuer()}}})
	})
	mux.HandleFunc(f.IssuerPath+"/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"issuer": issuer(),
			"authorization_endpoint": issuer() + "/authorize", "token_endpoint": issuer() + "/token", "end_session_endpoint": issuer() + "/logout"})
	})
	// The "browser" side: authorize auto-approves and redirects with a code.
	mux.HandleFunc(f.IssuerPath+"/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.LastAuth = q
		if q.Get("client_id") != f.ClientID || q.Get("redirect_uri") != f.Redirect {
			http.Error(w, "unknown client or redirect", 400) // what a real IdP shows its user
			return
		}
		f.n++
		code := fmt.Sprintf("code-%d", f.n)
		f.codes[code] = codeInfo{q.Get("code_challenge"), q.Get("redirect_uri"), q.Get("client_id")}
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), 302)
	})
	mux.HandleFunc(f.IssuerPath+"/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		fail := func(e string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": e, "error_description": "nope"})
		}
		f.n++
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			ci, ok := f.codes[r.Form.Get("code")]
			if !ok || ci.redirect != r.Form.Get("redirect_uri") || ci.client != r.Form.Get("client_id") {
				fail("invalid_grant")
				return
			}
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != ci.challenge {
				fail("invalid_grant") // PKCE verification failed
				return
			}
			delete(f.codes, r.Form.Get("code")) // single use
		case "refresh_token":
			if !f.refresh[r.Form.Get("refresh_token")] {
				fail("invalid_grant")
				return
			}
			delete(f.refresh, r.Form.Get("refresh_token")) // rotation
			f.Refreshes++
		default:
			fail("unsupported_grant_type")
			return
		}
		rt := fmt.Sprintf("refresh-%d", f.n)
		f.refresh[rt] = true
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("access-%d", f.n), "refresh_token": rt,
			"token_type": "Bearer", "expires_in": f.AccessTTL, "id_token": "idtoken"})
	})
	return f
}

// browse plays the browser: GET the authorization URL and return where the IdP redirected.
func browse(t *testing.T, authURL string) string {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 302 {
		t.Fatalf("authorize answered HTTP %d", resp.StatusCode)
	}
	return resp.Header.Get("Location")
}
