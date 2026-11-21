package oidc

import (
	"context"
	"fmt"
	"net"
	"net/http"
)

// Loopback receives the login redirect on 127.0.0.1 (RFC 8252 section 7.3),
// for desktop apps that have no custom URL scheme. The provider must allow the
// redirect URI (many allow any port for loopback addresses; some need the
// exact one registered, in which case register the printed URI pattern).
type Loopback struct {
	ln   net.Listener
	srv  *http.Server
	got  chan string
	base string
}

// NewLoopback starts listening on a random free port.
func NewLoopback() (*Loopback, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	l := &Loopback{ln: ln, got: make(chan string, 1), base: "http://" + ln.Addr().String()}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		select {
		case l.got <- l.base + r.URL.RequestURI():
		default:
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><html><body><p>Login complete. You can close this window.</p></body></html>")
	})
	l.srv = &http.Server{Handler: mux}
	go l.srv.Serve(ln) //nolint:errcheck // stops on Close
	return l, nil
}

// RedirectURI is the value to put in Config.RedirectURI.
func (l *Loopback) RedirectURI() string { return l.base + "/callback" }

// Wait blocks until the browser is redirected back (or ctx ends) and returns
// the full callback URL for Session.Complete.
func (l *Loopback) Wait(ctx context.Context) (string, error) {
	select {
	case u := <-l.got:
		return u, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Close stops the listener.
func (l *Loopback) Close() error { return l.srv.Close() }
