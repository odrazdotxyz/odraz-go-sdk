package dav

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
)

func newClient(t *testing.T, h http.Handler, mod func(*Config)) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := Config{BaseURL: srv.URL + "/dav"}
	if mod != nil {
		mod(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func TestURLEscaping(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://h/dav/"})
	got := c.URL("/a b/ü#?%.txt")
	if got != "http://h/dav/a%20b/%C3%BC%23%3F%25.txt" {
		t.Errorf("URL = %s", got)
	}
	if c.URL("/") != "http://h/dav/" || c.URL("") != "http://h/dav/" {
		t.Error("root URL wrong")
	}
}

func TestAuthAndHeaders(t *testing.T) {
	var got http.Header
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone() })
	c, _ := newClient(t, h, func(cfg *Config) {
		cfg.Creds = accounts.StaticProvider{Creds: accounts.Credentials{Kind: accounts.CredBasic, Username: "u", Password: "p"}}
		cfg.Headers = map[string]string{"P-Access-Token-Id": "id", "P-Access-Token": "tok"}
	})
	resp, err := c.Do(context.Background(), Request{Method: "GET", Path: "/x"})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if u, p, ok := parseBasic(got); !ok || u != "u" || p != "p" {
		t.Errorf("basic auth missing: %v", got)
	}
	if got.Get("P-Access-Token-Id") != "id" || got.Get("P-Access-Token") != "tok" {
		t.Errorf("proxy headers missing: %v", got)
	}
	c, _ = newClient(t, h, func(cfg *Config) {
		cfg.Creds = accounts.StaticProvider{Creds: accounts.Credentials{Kind: accounts.CredBearer, AccessToken: "tk"}}
	})
	resp, _ = c.Do(context.Background(), Request{Method: "GET", Path: "/x"})
	resp.Body.Close()
	if got.Get("Authorization") != "Bearer tk" {
		t.Errorf("bearer: %q", got.Get("Authorization"))
	}
}

func parseBasic(h http.Header) (string, string, bool) {
	r := &http.Request{Header: h}
	return r.BasicAuth()
}

func TestStatusMapping(t *testing.T) {
	for status, want := range map[int]error{401: odraz.ErrAuth, 403: odraz.ErrPermission, 404: odraz.ErrNotFound, 410: odraz.ErrNotFound, 412: odraz.ErrExists, 501: odraz.ErrUnsupported} {
		c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }), nil)
		_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/x"})
		if !errors.Is(err, want) || Status(err) != status {
			t.Errorf("status %d: err=%v", status, err)
		}
	}
	// Ambiguous statuses carry no sentinel but expose the code.
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(409) }), nil)
	_, err := c.Do(context.Background(), Request{Method: "MKCOL", Path: "/x"})
	if Status(err) != 409 || errors.Is(err, odraz.ErrNotFound) {
		t.Errorf("409: %v", err)
	}
}

func TestCrossHostRedirectIsBlocked(t *testing.T) {
	login := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html>login</html>")
	}))
	defer login.Close()
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, login.URL+"/login", http.StatusFound)
	}), nil)
	_, err := c.Do(context.Background(), Request{Method: "PROPFIND", Path: "/", Expect: ExpectXML})
	if !errors.Is(err, odraz.ErrBlockedByProxy) || !strings.Contains(err.Error(), "redirected") {
		t.Fatalf("err = %v", err)
	}
}

func TestSameHostRedirectFollowed(t *testing.T) {
	var methods []string
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/dav/old" {
			http.Redirect(w, r, "/dav/new", http.StatusMovedPermanently)
			return
		}
		body, _ := io.ReadAll(r.Body)
		io.WriteString(w, string(body))
	}), nil)
	resp, err := c.Do(context.Background(), Request{Method: "PUT", Path: "/old", Body: strings.NewReader("payload")})
	if err == nil {
		resp.Body.Close()
		t.Fatal("streaming body cannot be replayed; want error")
	}
	resp, err = c.Do(context.Background(), Request{Method: "GET", Path: "/old"})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(methods) != 3 || methods[2] != "GET /dav/new" {
		t.Errorf("methods = %v", methods)
	}
}

func TestHTMLInsteadOfXML(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, "<!DOCTYPE html><html>Sign in</html>")
	}), nil)
	_, err := c.Propfind(context.Background(), "/", 0)
	if !errors.Is(err, odraz.ErrBlockedByProxy) {
		t.Fatalf("err = %v", err)
	}
	// Same page with a non-HTML content type is still caught by sniffing.
	c, _ = newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, "<html>Sign in</html>")
	}), nil)
	if _, err = c.Propfind(context.Background(), "/", 0); !errors.Is(err, odraz.ErrBlockedByProxy) {
		t.Fatalf("sniffed: err = %v", err)
	}
}

func TestProxyLoginWithStatus401IsAuth(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, "<html>unauthorized</html>")
	}), nil)
	_, err := c.Propfind(context.Background(), "/", 0)
	if !errors.Is(err, odraz.ErrAuth) {
		t.Fatalf("err = %v", err)
	}
}

const apacheSample = `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:">
<D:response xmlns:lp1="DAV:" xmlns:lp2="http://apache.org/dav/props/">
<D:href>/dav/d/</D:href>
<D:propstat><D:prop><lp1:resourcetype><D:collection/></lp1:resourcetype>
<lp1:getlastmodified>Sat, 21 Nov 2026 11:30:38 GMT</lp1:getlastmodified><lp1:getetag>W/"50-1"</lp1:getetag></D:prop>
<D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
<D:response xmlns:lp1="DAV:">
<D:href>/dav/d/%c3%bcn%20caf%c3%a9.txt</D:href>
<D:propstat><D:prop><lp1:resourcetype/><lp1:getcontentlength>42</lp1:getcontentlength>
<lp1:getlastmodified>Sat, 21 Nov 2026 11:30:38 GMT</lp1:getlastmodified></D:prop>
<D:status>HTTP/1.1 200 OK</D:status></D:propstat>
<D:propstat><D:prop><D:quota-used-bytes/></D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat></D:response>
<D:response><D:href>http://example.com/dav/gone</D:href><D:status>HTTP/1.1 404 Not Found</D:status></D:response>
</D:multistatus>`

func TestParseMultistatus(t *testing.T) {
	rs, err := ParseMultistatus([]byte(apacheSample), "/dav")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("got %+v", rs)
	}
	if rs[0].Path != "/d" || !rs[0].IsDir || rs[0].ETag != `W/"50-1"` || rs[0].ModTime.IsZero() {
		t.Errorf("dir: %+v", rs[0])
	}
	if rs[1].Path != "/d/ün café.txt" || rs[1].IsDir || rs[1].Size != 42 || rs[1].QuotaUsed != -1 {
		t.Errorf("file: %+v", rs[1])
	}
	if _, err := ParseMultistatus([]byte(`<multistatus xmlns="DAV:"><response><href>/other/x</href><propstat><prop/><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`), "/dav"); err == nil {
		t.Error("href outside base must error")
	}
	// Absolute hrefs, root, base without path.
	rs, err = ParseMultistatus([]byte(`<multistatus xmlns="DAV:"><response><href>http://h/</href><propstat><prop><resourcetype><collection/></resourcetype><quota-used-bytes>5</quota-used-bytes><quota-available-bytes>95</quota-available-bytes></prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`), "")
	if err != nil || len(rs) != 1 || rs[0].Path != "/" || rs[0].QuotaUsed != 5 || rs[0].QuotaAvailable != 95 {
		t.Errorf("root: %+v %v", rs, err)
	}
}

func TestPropfindEndToEnd(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" || r.Header.Get("Depth") != "1" {
			t.Errorf("method=%s depth=%s", r.Method, r.Header.Get("Depth"))
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(207)
		io.WriteString(w, apacheSample)
	}), nil)
	rs, err := c.Propfind(context.Background(), "/d", 1)
	if err != nil || len(rs) != 2 {
		t.Fatalf("%v %v", rs, err)
	}
	if _, err := c.Propfind(context.Background(), "/d", 2); err == nil {
		t.Error("depth 2 must be rejected")
	}
}

func TestPropfindNon207(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "hello")
	}), nil)
	if _, err := c.Propfind(context.Background(), "/", 0); err == nil || errors.Is(err, odraz.ErrBlockedByProxy) {
		t.Errorf("plain 200 should be a generic error, got %v", err)
	}
}
