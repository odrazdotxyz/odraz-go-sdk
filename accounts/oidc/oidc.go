// Package oidc implements OpenID Connect login (authorization code flow with
// PKCE, public client) and token refresh for any adapter whose server trusts
// an OIDC provider. It has no user database: tokens go to the app's
// accounts.SecretStore, nothing else is stored.
//
// The SDK never opens a browser. The app does:
//
//	sess, _ := oidc.Begin(ctx, nil, "https://cloud.example.org", oidc.Config{ClientID: "...", RedirectURI: "..."})
//	// open sess.AuthorizationURL in the system browser; the redirect delivers callbackURL
//	tokens, err := sess.Complete(ctx, callbackURL)
//	err = oidc.Save(ctx, store, accountID, tokens)
//
// ID tokens are not verified: the tokens are received directly from the
// provider's token endpoint over TLS (OIDC Core 3.1.3.7), and the resource
// server validates the access token on every request.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
)

// DefaultScopes are the smallest scopes that work with OpenCloud: openid, the
// profile claims it maps users by (preferred_username), and a refresh token.
var DefaultScopes = []string{"openid", "profile", "offline_access"}

// Config identifies the app to the provider. Both values must be registered
// with the provider by its admin (once per server).
type Config struct {
	ClientID    string
	RedirectURI string
	Scopes      []string // DefaultScopes if empty
}

// Provider holds the discovered endpoints.
type Provider struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	EndSessionEndpoint    string `json:"end_session_endpoint,omitempty"`
}

// Tokens are the result of a login or refresh.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	Expiry       time.Time // zero if the provider gave no lifetime
}

func client(hc *http.Client) *http.Client {
	if hc != nil {
		return hc
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Discover finds the OIDC issuer of a server through WebFinger
// (/.well-known/webfinger), falling back to the server URL itself, and reads
// its openid-configuration.
func Discover(ctx context.Context, hc *http.Client, serverURL string) (*Provider, error) {
	hc = client(hc)
	su, err := url.Parse(serverURL)
	if err != nil || su.Scheme == "" || su.Host == "" {
		return nil, fmt.Errorf("oidc: invalid server URL %q", serverURL)
	}
	issuer := strings.TrimRight(serverURL, "/")
	wf := url.URL{Scheme: su.Scheme, Host: su.Host, Path: "/.well-known/webfinger"}
	q := url.Values{"resource": {issuer}, "rel": {"http://openid.net/specs/connect/1.0/issuer"}}
	wf.RawQuery = q.Encode()
	var jrd struct {
		Links []struct{ Rel, Href string } `json:"links"`
	}
	if err := getJSON(ctx, hc, wf.String(), &jrd); err == nil {
		for _, l := range jrd.Links {
			if l.Rel == "http://openid.net/specs/connect/1.0/issuer" && l.Href != "" {
				issuer = strings.TrimRight(l.Href, "/")
				break
			}
		}
	} else if errors.Is(err, odraz.ErrBlockedByProxy) || errors.Is(err, odraz.ErrAuth) {
		return nil, err
	}
	var p Provider
	if err := getJSON(ctx, hc, issuer+"/.well-known/openid-configuration", &p); err != nil {
		return nil, fmt.Errorf("oidc: discovering %s: %w", issuer, err)
	}
	if strings.TrimRight(p.Issuer, "/") != issuer {
		return nil, fmt.Errorf("oidc: issuer mismatch: asked %q, configuration says %q", issuer, p.Issuer)
	}
	if p.AuthorizationEndpoint == "" || p.TokenEndpoint == "" {
		return nil, fmt.Errorf("oidc: %s has no authorization or token endpoint", issuer)
	}
	return &p, nil
}

// Session is one login attempt. It can be saved with Marshal while the user is
// in the browser (the app may be suspended) and restored with Resume.
type Session struct {
	Provider         Provider `json:"provider"`
	ClientID         string   `json:"client_id"`
	RedirectURI      string   `json:"redirect_uri"`
	State            string   `json:"state"`
	Verifier         string   `json:"verifier"`
	AuthorizationURL string   `json:"authorization_url"` // open this in a browser

	hc *http.Client
}

// Begin discovers the provider and prepares a login with a fresh state and
// PKCE verifier.
func Begin(ctx context.Context, hc *http.Client, serverURL string, cfg Config) (*Session, error) {
	if cfg.ClientID == "" || cfg.RedirectURI == "" {
		return nil, errors.New("oidc: ClientID and RedirectURI are required")
	}
	p, err := Discover(ctx, hc, serverURL)
	if err != nil {
		return nil, err
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = DefaultScopes
	}
	s := &Session{Provider: *p, ClientID: cfg.ClientID, RedirectURI: cfg.RedirectURI, State: random(24), Verifier: random(48), hc: hc}
	sum := sha256.Sum256([]byte(s.Verifier))
	au, err := url.Parse(p.AuthorizationEndpoint)
	if err != nil {
		return nil, fmt.Errorf("oidc: bad authorization endpoint: %w", err)
	}
	q := au.Query()
	q.Set("response_type", "code")
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", cfg.RedirectURI)
	q.Set("scope", strings.Join(scopes, " "))
	q.Set("state", s.State)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("code_challenge_method", "S256")
	au.RawQuery = q.Encode()
	s.AuthorizationURL = au.String()
	return s, nil
}

// Marshal serializes the session (it contains the PKCE verifier: keep it as
// private as a secret until the login completes).
func (s *Session) Marshal() ([]byte, error) { return json.Marshal(s) }

// Resume restores a session saved with Marshal.
func Resume(data []byte, hc *http.Client) (*Session, error) {
	var s Session
	if err := json.Unmarshal(data, &s); err != nil || s.State == "" || s.Verifier == "" {
		return nil, errors.New("oidc: invalid saved session")
	}
	s.hc = hc
	return &s, nil
}

// Complete validates the redirect the browser delivered and exchanges the
// authorization code for tokens. Provider errors (access denied, ...) and a
// wrong state return errors matching odraz.ErrAuth.
func (s *Session) Complete(ctx context.Context, callbackURL string) (Tokens, error) {
	cu, err := url.Parse(callbackURL)
	if err != nil {
		return Tokens{}, fmt.Errorf("oidc: bad callback URL: %w", err)
	}
	q := cu.Query()
	if e := q.Get("error"); e != "" {
		return Tokens{}, fmt.Errorf("oidc: login failed: %s (%s): %w", e, q.Get("error_description"), odraz.ErrAuth)
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(s.State)) != 1 {
		return Tokens{}, fmt.Errorf("oidc: state mismatch, the login response does not belong to this attempt: %w", odraz.ErrAuth)
	}
	code := q.Get("code")
	if code == "" {
		return Tokens{}, fmt.Errorf("oidc: callback has no authorization code: %w", odraz.ErrAuth)
	}
	return tokenRequest(ctx, client(s.hc), s.Provider.TokenEndpoint, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {s.RedirectURI},
		"client_id":     {s.ClientID},
		"code_verifier": {s.Verifier},
	}, "")
}

// Save stores the tokens where accounts.Open reads them. Call it after Complete.
func Save(ctx context.Context, store accounts.SecretStore, accountID string, t Tokens) error {
	put := func(k, v string) error { return store.Put(ctx, accountID, k, []byte(v)) }
	if err := put(accounts.SecretAccessToken, t.AccessToken); err != nil {
		return err
	}
	if t.RefreshToken != "" {
		if err := put(accounts.SecretRefreshToken, t.RefreshToken); err != nil {
			return err
		}
	}
	exp := ""
	if !t.Expiry.IsZero() {
		exp = t.Expiry.UTC().Format(time.RFC3339)
	}
	return put(accounts.SecretExpiry, exp)
}

// RefreshFunc returns an accounts.RefreshFunc that renews tokens with the
// refresh-token grant. The provider is discovered on first use and cached.
func RefreshFunc(hc *http.Client, serverURL, clientID string) accounts.RefreshFunc {
	var (
		mu   sync.Mutex
		prov *Provider
	)
	return func(ctx context.Context, old accounts.Credentials) (accounts.Credentials, error) {
		mu.Lock()
		if prov == nil {
			p, err := Discover(ctx, hc, serverURL)
			if err != nil {
				mu.Unlock()
				return accounts.Credentials{}, err // retried on the next refresh
			}
			prov = p
		}
		p := prov
		mu.Unlock()
		t, err := tokenRequest(ctx, client(hc), p.TokenEndpoint, url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {old.RefreshToken},
			"client_id":     {clientID},
		}, old.RefreshToken)
		if err != nil {
			return accounts.Credentials{}, err
		}
		return accounts.Credentials{Kind: accounts.CredBearer, AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, Expiry: t.Expiry}, nil
	}
}

// EndSessionURL returns the provider's logout URL, or "" if it has none.
func (p *Provider) EndSessionURL(idTokenHint, postLogoutRedirect string) string {
	if p.EndSessionEndpoint == "" {
		return ""
	}
	u, err := url.Parse(p.EndSessionEndpoint)
	if err != nil {
		return ""
	}
	q := u.Query()
	if idTokenHint != "" {
		q.Set("id_token_hint", idTokenHint)
	}
	if postLogoutRedirect != "" {
		q.Set("post_logout_redirect_uri", postLogoutRedirect)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func tokenRequest(ctx context.Context, hc *http.Client, endpoint string, form url.Values, keepRefresh string) (Tokens, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Tokens{}, ctx.Err()
		}
		return Tokens{}, fmt.Errorf("oidc: token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if isHTML(resp.Header.Get("Content-Type"), body) {
		return Tokens{}, fmt.Errorf("%w: the token endpoint returned an HTML page", odraz.ErrBlockedByProxy)
	}
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
		Description  string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &tr)
	if resp.StatusCode != http.StatusOK || tr.Error != "" || tr.AccessToken == "" {
		msg := fmt.Sprintf("oidc: token endpoint answered HTTP %d: %s %s", resp.StatusCode, tr.Error, tr.Description)
		switch tr.Error {
		case "invalid_grant", "invalid_client", "unauthorized_client", "access_denied", "invalid_request":
			return Tokens{}, fmt.Errorf("%s: %w", strings.TrimSpace(msg), odraz.ErrAuth)
		}
		if resp.StatusCode == 400 || resp.StatusCode == 401 {
			return Tokens{}, fmt.Errorf("%s: %w", strings.TrimSpace(msg), odraz.ErrAuth)
		}
		return Tokens{}, errors.New(strings.TrimSpace(msg))
	}
	t := Tokens{AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken, IDToken: tr.IDToken}
	if t.RefreshToken == "" {
		t.RefreshToken = keepRefresh
	}
	if tr.ExpiresIn > 0 {
		t.Expiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return t, nil
}

func getJSON(ctx context.Context, hc *http.Client, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return fmt.Errorf("GET %s: HTTP %d: %w", u, resp.StatusCode, odraz.ErrAuth)
	case resp.StatusCode == 404:
		return fmt.Errorf("GET %s: %w", u, odraz.ErrNotFound)
	case resp.StatusCode >= 300:
		return fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	if isHTML(resp.Header.Get("Content-Type"), body) {
		return fmt.Errorf("%w: GET %s returned an HTML page instead of JSON", odraz.ErrBlockedByProxy, u)
	}
	return json.Unmarshal(body, out)
}

func isHTML(ct string, body []byte) bool {
	if mt, _, err := mime.ParseMediaType(ct); err == nil && (mt == "text/html" || mt == "application/xhtml+xml") {
		return true
	}
	p := strings.ToLower(strings.TrimSpace(string(body[:min(len(body), 64)])))
	return strings.HasPrefix(p, "<!doctype html") || strings.HasPrefix(p, "<html")
}

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // no entropy: nothing safe to do
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HTTPClient returns an HTTP client for talking to the provider that honours
// the account's connection options: extra headers (auth proxies) and TLS
// settings (client certificate, self-signed certificates).
func HTTPClient(env *accounts.Env) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = env.TLS
	return &http.Client{Timeout: 30 * time.Second, Transport: headerTransport{tr, env.Headers}}
}

type headerTransport struct {
	rt      http.RoundTripper
	headers map[string]string
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t.headers {
		r.Header.Set(k, v)
	}
	return t.rt.RoundTrip(r)
}
