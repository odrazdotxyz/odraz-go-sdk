// Package idptest holds test helpers for real identity providers used by the
// SDK's own tests (Pocket ID). Not part of the public API.
package idptest

import (
	"bytes"
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
)

// PocketID drives a Pocket ID test instance (testenv/pocketid or testenv/opencloud-pocketid).
type PocketID struct {
	T      *testing.T
	URL    string
	APIKey string
	HC     *http.Client
}

// New returns the instance named by ODRAZ_TEST_POCKETID_*, or skips the test.
func New(t *testing.T) *PocketID {
	u, k := os.Getenv("ODRAZ_TEST_POCKETID_URL"), os.Getenv("ODRAZ_TEST_POCKETID_API_KEY")
	if u == "" || k == "" {
		t.Skip("ODRAZ_TEST_POCKETID_* not set (testenv/pocketid/seed.sh)")
	}
	return &PocketID{T: t, URL: u, APIKey: k, HC: &http.Client{Timeout: 20 * time.Second}}
}

// Admin calls the admin API with the static API key.
func (p *PocketID) Admin(method, path string, body, out any) {
	p.T.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, p.URL+path, r)
	req.Header.Set("X-API-KEY", p.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.HC.Do(req)
	if err != nil {
		p.T.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		p.T.Fatalf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			p.T.Fatalf("%s %s: %v: %s", method, path, err, raw)
		}
	}
}

// Fixture is a fresh user and a public PKCE client created for one test.
type Fixture struct{ Username, UserID, ClientID string }

// setup creates a fresh user and a public PKCE client that allows loopback redirects.
// Setup creates a user and a client that allows loopback redirects; both are deleted at test end.
func (p *PocketID) Setup() Fixture {
	n := rand.Int63()
	f := Fixture{Username: fmt.Sprintf("alice%d", n), ClientID: fmt.Sprintf("odraz-%d", n)}
	var u struct{ ID string }
	p.Admin("POST", "/api/users", map[string]any{"username": f.Username, "email": f.Username + "@example.test", "emailVerified": true,
		"firstName": "Alice", "lastName": "Test", "displayName": "Alice Test"}, &u)
	f.UserID = u.ID
	p.Admin("POST", "/api/oidc/clients", map[string]any{"id": f.ClientID, "name": "Odraz SDK test",
		"callbackURLs": []string{"http://127.0.0.1/callback", "http://localhost/callback"}, "isPublic": true, "pkceEnabled": true}, nil)
	p.T.Cleanup(func() {
		p.Admin("DELETE", "/api/oidc/clients/"+f.ClientID, nil, nil)
		p.Admin("DELETE", "/api/users/"+f.UserID, nil, nil)
	})
	return f
}

// browser is a logged-in browser session (via one-time access token).
// EnsureUser returns the ID of the user with this username, creating it (with an email) if needed.
// Unlike Setup it leaves the user in place, for tests about users that already exist elsewhere.
func (p *PocketID) EnsureUser(username, email string) string {
	var list struct {
		Data []struct{ ID, Username string } `json:"data"`
	}
	p.Admin("GET", "/api/users?search="+url.QueryEscape(username), nil, &list)
	for _, u := range list.Data {
		if u.Username == username {
			return u.ID
		}
	}
	var u struct{ ID string }
	p.Admin("POST", "/api/users", map[string]any{"username": username, "email": email, "emailVerified": true,
		"firstName": "Existing", "lastName": "User", "displayName": "Existing User"}, &u)
	return u.ID
}

// EnsureClient creates the loopback PKCE test client with this ID if it does not exist yet.
func (p *PocketID) EnsureClient(id string) {
	body := map[string]any{"id": id, "name": "Odraz SDK test",
		"callbackURLs": []string{"http://127.0.0.1/callback", "http://localhost/callback"}, "isPublic": true, "pkceEnabled": true}
	req, _ := http.NewRequest("GET", p.URL+"/api/oidc/clients/"+id, nil)
	req.Header.Set("X-API-KEY", p.APIKey)
	resp, err := p.HC.Do(req)
	if err != nil {
		p.T.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		p.Admin("POST", "/api/oidc/clients", body, nil)
	}
}

// Browser returns a logged-in browser session (one-time access token instead of a passkey).
func (p *PocketID) Browser(userID string) *http.Client {
	var tok struct{ Token string }
	p.Admin("POST", "/api/users/"+userID+"/one-time-access-token", map[string]any{"ttl": "15m"}, &tok)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Post(p.URL+"/api/one-time-access-token/"+tok.Token, "application/json", strings.NewReader("{}"))
	if err != nil || resp.StatusCode != 200 {
		p.T.Fatalf("exchanging the one-time access token: %v %v", resp, err)
	}
	resp.Body.Close()
	return c
}

// authorize plays the browser through /authorize, completing the consent
// interaction if asked, and finally follows the redirect to the app (the
// loopback listener). Returns the HTTP status of the final hop.
// Authorize plays the browser through the authorization URL, completing the consent
// interaction, and delivers the final redirect to the app (e.g. a oidc.Loopback).
func (p *PocketID) Authorize(b *http.Client, authURL string) error {
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
				r, err := b.Post(p.URL+"/api/oidc/interactions/"+id+"/complete", "application/json", bytes.NewReader(body))
				if err != nil {
					return err
				}
				r.Body.Close()
				if r.StatusCode != 200 {
					return fmt.Errorf("completing %q: HTTP %d", step, r.StatusCode)
				}
			}
			next = p.URL + "/authorize?interaction=" + id
		case strings.HasPrefix(abs.String(), p.URL):
			next = abs.String()
		default: // the app's redirect URI: deliver it like a browser would
			r, err := p.HC.Get(abs.String())
			if err != nil {
				return err
			}
			r.Body.Close()
			return nil
		}
	}
	return errors.New("too many redirects in the authorize flow")
}

func (p *PocketID) getJSON(b *http.Client, path string, out any) error {
	resp, err := b.Get(p.URL + path)
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
