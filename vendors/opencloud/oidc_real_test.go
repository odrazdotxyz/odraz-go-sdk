package opencloud_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts/oidc"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
)

// headlessBrowser performs the login the way OpenCloud's sign-in page does
// (its built-in Konnect IdP): authorize -> logon -> consent -> authorize with
// the "konnect" continuation parameter. The protocol was read from the page's
// own scripts; the requests need Origin/Referer headers like a browser sends.
type headlessBrowser struct {
	t      *testing.T
	server string
	c      *http.Client
}

func newBrowser(t *testing.T, server string) *headlessBrowser {
	jar, _ := cookiejar.New(nil)
	return &headlessBrowser{t: t, server: server, c: &http.Client{
		Jar:           jar,
		Transport:     &http.Transport{TLSClientConfig: insecureTLS()},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (b *headlessBrowser) post(path string, body any, out any) error {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", b.server+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Kopano-Konnect-XSRF", "1")
	req.Header.Set("Origin", b.server)
	req.Header.Set("Referer", b.server+"/signin/v1/identifier")
	resp, err := b.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return json.Unmarshal(raw, out)
}

// login returns the callback URL the browser would be redirected to.
func (b *headlessBrowser) login(authURL, user, pass string) (string, error) {
	au, _ := url.Parse(authURL)
	q := au.Query()
	scope, client, redirect, state := q.Get("scope"), q.Get("client_id"), q.Get("redirect_uri"), q.Get("state")

	var lr struct {
		Hello struct {
			Success bool            `json:"success"`
			Next    string          `json:"next"`
			Scopes  map[string]bool `json:"scopes"`
		} `json:"hello"`
		State string `json:"state"`
	}
	err := b.post("/signin/v1/identifier/_/logon", map[string]any{
		"state":  "logon-state",
		"params": []string{user, pass, "1"},
		"hello":  map[string]string{"scope": scope, "client_id": client, "redirect_uri": redirect, "flow": "oidc"},
	}, &lr)
	if err != nil {
		return "", fmt.Errorf("logon: %w", err)
	}
	if !lr.Hello.Success {
		return "", errors.New("logon refused (wrong user or password)")
	}
	konnect := lr.State
	if lr.Hello.Next == "consent" {
		var granted []string
		for s, ok := range lr.Hello.Scopes {
			if ok {
				granted = append(granted, s)
			}
		}
		sort.Strings(granted)
		var cr struct {
			Success bool   `json:"success"`
			State   string `json:"state"`
		}
		if err := b.post("/signin/v1/identifier/_/consent", map[string]any{
			"state": "consent-state", "allow": true, "scope": strings.Join(granted, " "),
			"client_id": client, "redirect_uri": redirect, "ref": state, "flow_nonce": "",
		}, &cr); err != nil {
			return "", fmt.Errorf("consent: %w", err)
		}
		konnect = cr.State
	}
	q.Set("konnect", konnect)
	au.Path = "/signin/v1/identifier/_/authorize"
	au.RawQuery = q.Encode()
	resp, err := b.c.Get(au.String())
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != 302 || !strings.HasPrefix(loc, redirect) {
		return "", fmt.Errorf("authorize did not redirect to the app (HTTP %d, Location %q)", resp.StatusCode, loc)
	}
	return loc, nil
}

type realOIDC struct{ url, user, pass, client string }

func realOIDCEnv(t *testing.T) realOIDC {
	e := realOIDC{os.Getenv("ODRAZ_TEST_OPENCLOUD_URL"), os.Getenv("ODRAZ_TEST_OPENCLOUD_USER"),
		os.Getenv("ODRAZ_TEST_OPENCLOUD_PASSWORD"), os.Getenv("ODRAZ_TEST_OPENCLOUD_OIDC_CLIENT")}
	if e.url == "" || e.pass == "" || e.client == "" {
		t.Skip("ODRAZ_TEST_OPENCLOUD_{URL,PASSWORD,OIDC_CLIENT} not set (testenv/opencloud/seed.sh)")
	}
	return e
}

const redirect = "http://127.0.0.1:5000/callback"

// doLogin runs the SDK's oidc.Begin/Complete against the real IdP.
func doLogin(t *testing.T, e realOIDC, hc *http.Client) (oidc.Tokens, error) {
	sess, err := oidc.Begin(ctx, hc, e.url, oidc.Config{ClientID: e.client, RedirectURI: redirect})
	if err != nil {
		return oidc.Tokens{}, fmt.Errorf("begin: %w", err)
	}
	cb, err := newBrowser(t, e.url).login(sess.AuthorizationURL, e.user, e.pass)
	if err != nil {
		return oidc.Tokens{}, err
	}
	return sess.Complete(ctx, cb)
}

func realHC(t *testing.T, e realOIDC) *http.Client {
	st := accounts.NewMemoryStore()
	h, err := accounts.Open(ctx, accounts.Account{ID: "x", Adapter: "opencloud", Config: map[string]string{"url": e.url, accounts.ConfigInsecure: "true"}}, st)
	if err != nil {
		t.Fatal(err)
	}
	return oidc.HTTPClient(h.Env)
}

func openOIDCAccount(t *testing.T, e realOIDC, store accounts.SecretStore) files.Client {
	acc := accounts.Account{ID: "oc", Adapter: "opencloud", Config: map[string]string{
		"url": e.url, accounts.ConfigInsecure: "true", accounts.ConfigOIDCClientID: e.client}}
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

func TestRealOIDCLoginAndRefresh(t *testing.T) {
	e := realOIDCEnv(t)
	hc := realHC(t, e)

	// Discovery against the real server.
	p, err := oidc.Discover(ctx, hc, e.url)
	if err != nil || !strings.Contains(p.TokenEndpoint, "/konnect/v1/token") {
		t.Fatalf("discover: %+v %v", p, err)
	}

	// Login with a registered third-party client, then use the token on the API.
	tokens, err := doLogin(t, e, hc)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" || time.Until(tokens.Expiry) <= 0 {
		t.Fatalf("tokens = %+v (refresh token needs offline_access)", tokens)
	}
	store := accounts.NewMemoryStore()
	if err := oidc.Save(ctx, store, "oc", tokens); err != nil {
		t.Fatal(err)
	}
	cl := openOIDCAccount(t, e, store)
	drives, err := cl.Drives(ctx)
	if err != nil || len(drives) == 0 {
		t.Fatalf("drives with the OIDC token: %v %v", drives, err)
	}
	var personal string
	for _, d := range drives {
		if d.Kind == files.DrivePersonal {
			personal = d.ID
		}
	}
	fs, err := cl.OpenDrive(ctx, personal)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := fs.Stat(ctx, "/"); err != nil || !st.IsDir {
		t.Fatalf("WebDAV with the OIDC token: %+v %v", st, err)
	}

	// 1) Expired token: refreshed before the request, rotated tokens saved.
	oldAccess, _ := store.Get(ctx, "oc", accounts.SecretAccessToken)
	oldRefresh, _ := store.Get(ctx, "oc", accounts.SecretRefreshToken)
	store.Put(ctx, "oc", accounts.SecretExpiry, []byte(time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)))
	if _, err := openOIDCAccount(t, e, store).Drives(ctx); err != nil {
		t.Fatalf("drives after expiry: %v", err)
	}
	newAccess, _ := store.Get(ctx, "oc", accounts.SecretAccessToken)
	newRefresh, _ := store.Get(ctx, "oc", accounts.SecretRefreshToken)
	if string(newAccess) == string(oldAccess) || string(newRefresh) == string(oldRefresh) {
		t.Error("expiry refresh must store a new access token and the rotated refresh token")
	}

	// 2) Revoked/invalid token that still looks valid: the server answers 401,
	//    the SDK refreshes once and retries.
	store.Put(ctx, "oc", accounts.SecretAccessToken, []byte("not-a-real-token"))
	store.Put(ctx, "oc", accounts.SecretExpiry, []byte(time.Now().Add(time.Hour).UTC().Format(time.RFC3339)))
	if _, err := openOIDCAccount(t, e, store).Drives(ctx); err != nil {
		t.Fatalf("a rejected token must be recovered with the refresh token: %v", err)
	}
	if v, _ := store.Get(ctx, "oc", accounts.SecretAccessToken); string(v) == "not-a-real-token" {
		t.Error("recovered token not saved")
	}

	// Observed, not asserted: OpenCloud's built-in IdP issues stateless refresh
	// tokens, so a rotated-away refresh token keeps working until it expires
	// (other IdPs reject it). See testenv/opencloud/README.md.
}

func TestRealOIDCRefusals(t *testing.T) {
	e := realOIDCEnv(t)
	hc := realHC(t, e)

	// A client the admin did not register is refused by the IdP.
	bad := e
	bad.client = "not-registered"
	if _, err := doLogin(t, bad, hc); err == nil || !strings.Contains(err.Error(), "unknown client_id") {
		t.Errorf("unregistered client: %v", err)
	}
	// Wrong password.
	wrong := e
	wrong.pass = "wrong-password"
	if _, err := doLogin(t, wrong, hc); err == nil {
		t.Error("wrong password must not log in")
	}
	// A code can only be used once, and only with its PKCE verifier.
	sess, _ := oidc.Begin(ctx, hc, e.url, oidc.Config{ClientID: e.client, RedirectURI: redirect})
	cb, err := newBrowser(t, e.url).login(sess.AuthorizationURL, e.user, e.pass)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Complete(ctx, cb); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Complete(ctx, cb); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("code replay: %v", err)
	}
}
