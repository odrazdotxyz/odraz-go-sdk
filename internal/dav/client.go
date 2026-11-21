// Package dav is the shared WebDAV transport used by the files adapters and,
// later, CalDAV/CardDAV. It owns the HTTP client so it can add auth and proxy
// headers, refuse cross-host redirects, detect HTML where XML/JSON is
// expected (odraz.ErrBlockedByProxy) and map HTTP statuses to sentinel errors.
package dav

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
)

const maxRedirects = 5

// Config configures a Client.
type Config struct {
	BaseURL string                      // e.g. https://host/remote.php/dav/files/me
	Creds   accounts.CredentialProvider // nil for anonymous
	Headers map[string]string           // extra headers on every request (auth proxies)
	TLS     *tls.Config                 // optional, e.g. client certificate (mTLS)
	// HTTPClient overrides the underlying client (tests). Its CheckRedirect
	// is replaced; redirects are handled by Client itself.
	HTTPClient *http.Client
	UserAgent  string
}

// Client is a WebDAV HTTP client. Safe for concurrent use.
type Client struct {
	base    *url.URL
	creds   accounts.CredentialProvider
	headers map[string]string
	hc      *http.Client
	ua      string
}

// Expect tells Do what the response body should be, for proxy detection.
type Expect int

const (
	ExpectAny Expect = iota
	ExpectXML
	ExpectJSON
)

// Request is one HTTP exchange. Path is relative to the base URL.
type Request struct {
	Method string
	Path   string
	URL    string // absolute URL; overrides Path when set
	Header http.Header
	Body   io.Reader
	Length int64 // body length, -1 unknown; ignored for nil bodies
	Expect Expect
}

// New creates a Client.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("dav: invalid base URL %q", cfg.BaseURL)
	}
	hc := &http.Client{}
	if cfg.HTTPClient != nil {
		c := *cfg.HTTPClient
		hc = &c
	} else {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = cfg.TLS
		hc.Transport = tr
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ua := cfg.UserAgent
	if ua == "" {
		ua = "odraz-sdk"
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	return &Client{base: u, creds: cfg.Creds, headers: cfg.Headers, hc: hc, ua: ua}, nil
}

// URL returns the absolute, correctly escaped URL for a path relative to the
// base. rel is "/"-separated and unescaped.
func (c *Client) URL(rel string) string {
	u := *c.base
	u.Path = c.base.Path + "/" + strings.TrimLeft(rel, "/")
	if rel == "" || rel == "/" {
		u.Path = c.base.Path + "/"
	}
	return u.String()
}

// Do sends the request and returns the response for any status below 400.
// Statuses 400 and above become an *Error (wrapping a sentinel where the
// meaning is unambiguous). The caller must close the response body.
func (c *Client) Do(ctx context.Context, r Request) (*http.Response, error) {
	target := r.URL
	if target == "" {
		target = c.URL(r.Path)
	}
	var getBody func() (io.Reader, error)
	body := r.Body
	switch b := r.Body.(type) {
	case nil:
	case *bytes.Reader:
		getBody = func() (io.Reader, error) { _, err := b.Seek(0, io.SeekStart); return b, err }
	}
	origHost := hostOf(c.base)
	for hop := 0; ; hop++ {
		req, err := http.NewRequestWithContext(ctx, r.Method, target, body)
		if err != nil {
			return nil, fmt.Errorf("dav: %w", err)
		}
		if r.Body != nil && r.Length >= 0 {
			req.ContentLength = r.Length
		}
		for k, v := range r.Header {
			req.Header[k] = v
		}
		if err := c.decorate(ctx, req); err != nil {
			return nil, err
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("dav: %s %s: %w", r.Method, target, err)
		}
		if isRedirect(resp.StatusCode) && resp.Header.Get("Location") != "" {
			loc, err := req.URL.Parse(resp.Header.Get("Location"))
			resp.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("dav: bad redirect location: %w", err)
			}
			if hostOf(loc) != origHost {
				return nil, fmt.Errorf("%w: %s %s was redirected to %s (a login or gateway page?)",
					odraz.ErrBlockedByProxy, r.Method, target, loc.Host)
			}
			if hop >= maxRedirects {
				return nil, fmt.Errorf("dav: too many redirects for %s", target)
			}
			if r.Body != nil {
				if getBody == nil {
					return nil, fmt.Errorf("dav: redirect to %s cannot replay a streaming body", loc)
				}
				if body, err = getBody(); err != nil {
					return nil, err
				}
			}
			target = loc.String()
			continue
		}
		if resp.StatusCode >= 400 {
			defer resp.Body.Close()
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			if isHTML(resp.Header.Get("Content-Type"), snippet) && r.Expect != ExpectAny && resp.StatusCode != 404 &&
				resp.StatusCode != 401 && resp.StatusCode != 403 {
				return nil, fmt.Errorf("%w: %s %s returned an HTML page (HTTP %d)", odraz.ErrBlockedByProxy, r.Method, target, resp.StatusCode)
			}
			return nil, newError(r.Method, target, resp.StatusCode, string(snippet))
		}
		if r.Expect != ExpectAny && isHTML(resp.Header.Get("Content-Type"), nil) {
			resp.Body.Close()
			return nil, fmt.Errorf("%w: %s %s returned an HTML page instead of %s",
				odraz.ErrBlockedByProxy, r.Method, target, map[Expect]string{ExpectXML: "XML", ExpectJSON: "JSON"}[r.Expect])
		}
		return resp, nil
	}
}

func (c *Client) decorate(ctx context.Context, req *http.Request) error {
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", c.ua)
	if c.creds == nil {
		return nil
	}
	cr, err := c.creds.Credentials(ctx)
	if err != nil {
		return fmt.Errorf("dav: credentials: %w", err)
	}
	switch cr.Kind {
	case accounts.CredBearer:
		req.Header.Set("Authorization", "Bearer "+cr.AccessToken)
	case accounts.CredBasic:
		req.SetBasicAuth(cr.Username, cr.Password)
	}
	return nil
}

func isRedirect(code int) bool {
	return code == 301 || code == 302 || code == 303 || code == 307 || code == 308
}

func hostOf(u *url.URL) string { return strings.ToLower(u.Host) }

// isHTML reports whether a content type (or, if given, a body prefix) is HTML.
func isHTML(contentType string, prefix []byte) bool {
	if mt, _, err := mime.ParseMediaType(contentType); err == nil && (mt == "text/html" || mt == "application/xhtml+xml") {
		return true
	}
	p := strings.ToLower(strings.TrimSpace(string(prefix)))
	return strings.HasPrefix(p, "<!doctype html") || strings.HasPrefix(p, "<html")
}

// Error is an HTTP failure. It wraps a sentinel when the status maps
// unambiguously; adapters inspect Status for the rest (405, 409, 500...).
type Error struct {
	Method, URL string
	Status      int
	Body        string
	err         error
}

func (e *Error) Error() string {
	return fmt.Sprintf("dav: %s %s: HTTP %d %s", e.Method, e.URL, e.Status, http.StatusText(e.Status))
}
func (e *Error) Unwrap() error { return e.err }

func newError(method, u string, status int, body string) *Error {
	e := &Error{Method: method, URL: u, Status: status, Body: body}
	switch status {
	case 401:
		e.err = odraz.ErrAuth
	case 403:
		e.err = odraz.ErrPermission
	case 404, 410:
		e.err = odraz.ErrNotFound
	case 412:
		e.err = odraz.ErrExists // failed Overwrite: F / If-None-Match
	case 501:
		e.err = odraz.ErrUnsupported
	}
	return e
}

// Status returns the HTTP status carried by err, or 0.
func Status(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}
