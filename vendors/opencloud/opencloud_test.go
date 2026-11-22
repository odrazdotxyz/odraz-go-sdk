package opencloud_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
	"github.com/odrazdotxyz/odraz-go-sdk/files/fstest"
	_ "github.com/odrazdotxyz/odraz-go-sdk/vendors/opencloud"
)

var ctx = context.Background()

func openClient(t *testing.T, url, user, token string, extra map[string]string) files.Client {
	t.Helper()
	st := accounts.NewMemoryStore()
	st.Put(ctx, "oc", accounts.SecretPassword, []byte(token))
	cfg := map[string]string{"url": url, "username": user}
	for k, v := range extra {
		cfg[k] = v
	}
	h, err := accounts.Open(ctx, accounts.Account{ID: "oc", Label: "OC", Adapter: "opencloud", Config: cfg}, st)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := files.ForAccount(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

func validate(url, user, token string) error {
	st := accounts.NewMemoryStore()
	st.Put(ctx, "oc", accounts.SecretPassword, []byte(token))
	return accounts.Validate(ctx, accounts.Account{ID: "oc", Adapter: "opencloud", Config: map[string]string{"url": url, "username": user}}, st)
}

const drivesJSON = `{"value":[
 {"id":"p$1","name":"Admin","driveType":"personal","quota":{"total":0,"used":10,"remaining":9223372036854775807},"root":{"webDavUrl":"https://public.example/dav/spaces/p$1"}},
 {"id":"j$2","name":"Team","driveType":"project","quota":{"total":1000,"used":5},"root":{"webDavUrl":"https://public.example/dav/spaces/j$2"}},
 {"id":"s$3","name":"Shares","driveType":"virtual","quota":null,"root":{"webDavUrl":"https://public.example/dav/spaces/s$3"}},
 {"id":"x$4","name":"Odd","driveType":"whatever","root":{"webDavUrl":""}}]}`

func fakeGraph(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, p, _ := r.BasicAuth(); p != "tok" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/graph/v1.0/me":
			io.WriteString(w, `{"id":"u1"}`)
		case "/graph/v1.0/me/drives":
			io.WriteString(w, drivesJSON)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDrivesMapping(t *testing.T) {
	srv := fakeGraph(t)
	ds, err := openClient(t, srv.URL, "u", "tok", nil).Drives(ctx)
	if err != nil || len(ds) != 4 {
		t.Fatalf("%v %v", ds, err)
	}
	want := []struct {
		id   string
		kind files.DriveKind
		q    *files.Quota
	}{
		{"p$1", files.DrivePersonal, &files.Quota{Used: 10, Total: -1}}, // total 0 = unlimited
		{"j$2", files.DriveProject, &files.Quota{Used: 5, Total: 1000}},
		{"s$3", files.DriveShared, nil},
		{"x$4", files.DriveOther, nil},
	}
	for i, w := range want {
		d := ds[i]
		if d.ID != w.id || d.Kind != w.kind || fmt.Sprint(d.Quota) != fmt.Sprint(w.q) {
			t.Errorf("drive %d = %+v quota=%v, want %+v", i, d, d.Quota, w)
		}
	}
}

func TestSharesDriveIsReadOnly(t *testing.T) {
	srv := fakeGraph(t)
	cl := openClient(t, srv.URL, "u", "tok", nil)
	ds, _ := cl.Drives(ctx)
	for _, d := range ds {
		if d.ReadOnly != (d.Kind == files.DriveShared) {
			t.Errorf("drive %s: ReadOnly=%v", d.ID, d.ReadOnly)
		}
	}
	fs, err := cl.OpenDrive(ctx, "s$3")
	if err != nil {
		t.Fatal(err)
	}
	if !fs.Capabilities().ReadOnly {
		t.Error("shares FS must report ReadOnly")
	}
	if err := fs.Mkdir(ctx, "/x"); !errors.Is(err, odraz.ErrPermission) {
		t.Errorf("mkdir on shares: %v", err)
	}
}

func TestOpenDriveUsesConfiguredHost(t *testing.T) {
	// The API says https://public.example/...; the client must keep talking to
	// the configured server and only take the path from the API.
	var hit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/graph/v1.0/me/drives":
			io.WriteString(w, drivesJSON)
		default:
			hit = r.Method + " " + r.URL.Path
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(207)
			io.WriteString(w, `<multistatus xmlns="DAV:"><response><href>/dav/spaces/p$1/</href><propstat><prop><resourcetype><collection/></resourcetype></prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`)
		}
	}))
	defer srv.Close()
	cl := openClient(t, srv.URL, "u", "tok", nil)
	fs, err := cl.OpenDrive(ctx, "p$1")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := fs.Stat(ctx, "/"); err != nil || !st.IsDir {
		t.Fatalf("stat root: %+v %v", st, err)
	}
	if hit != "PROPFIND /dav/spaces/p$1/" {
		t.Errorf("hit = %q", hit)
	}
	if _, err := cl.OpenDrive(ctx, "nope"); !errors.Is(err, odraz.ErrNotFound) {
		t.Errorf("unknown drive: %v", err)
	}
}

func TestValidate(t *testing.T) {
	srv := fakeGraph(t)
	if err := validate(srv.URL, "u", "tok"); err != nil {
		t.Errorf("valid: %v", err)
	}
	if err := validate(srv.URL, "u", "wrong"); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("bad token: %v", err)
	}
	// A server that is not OpenCloud.
	other := httptest.NewServer(http.NotFoundHandler())
	defer other.Close()
	if err := validate(other.URL, "u", "tok"); !errors.Is(err, odraz.ErrNotFound) {
		t.Errorf("not opencloud: %v", err)
	}
	// An auth proxy answering with its login page.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html>Sign in</html>")
	}))
	defer proxy.Close()
	if err := validate(proxy.URL, "u", "tok"); !errors.Is(err, odraz.ErrBlockedByProxy) {
		t.Errorf("proxy page: %v", err)
	}
	// And one that redirects to its login host.
	login := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer login.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, login.URL+"/login", 302)
	}))
	defer redir.Close()
	if err := validate(redir.URL, "u", "tok"); !errors.Is(err, odraz.ErrBlockedByProxy) {
		t.Errorf("redirect: %v", err)
	}
}

// Real server: eval "$(testenv/opencloud/seed.sh)" then go test ./vendors/opencloud
func realEnv(t *testing.T) (url, user, token string) {
	url, user, token = os.Getenv("ODRAZ_TEST_OPENCLOUD_URL"), os.Getenv("ODRAZ_TEST_OPENCLOUD_USER"), os.Getenv("ODRAZ_TEST_OPENCLOUD_TOKEN")
	if url == "" || token == "" {
		t.Skip("ODRAZ_TEST_OPENCLOUD_* not set")
	}
	return
}

var insecure = map[string]string{accounts.ConfigInsecure: "true"}

func TestRealValidate(t *testing.T) {
	url, user, token := realEnv(t)
	if err := validate(url, user, token); err == nil {
		t.Error("self-signed certificate must be rejected without the insecure option")
	}
	st := accounts.NewMemoryStore()
	st.Put(ctx, "oc", accounts.SecretPassword, []byte(token))
	acc := accounts.Account{ID: "oc", Adapter: "opencloud", Config: map[string]string{"url": url, "username": user, accounts.ConfigInsecure: "true"}}
	if err := accounts.Validate(ctx, acc, st); err != nil {
		t.Errorf("valid: %v", err)
	}
	st.Put(ctx, "oc", accounts.SecretPassword, []byte("wrong token"))
	if err := accounts.Validate(ctx, acc, st); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("bad token: %v", err)
	}
}

// ensureProject returns the ID of a project space, creating one if needed.
func ensureProject(t *testing.T, url, user, token string) string {
	cl := openClient(t, url, user, token, insecure)
	ds, err := cl.Drives(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if d.Kind == files.DriveProject {
			return d.ID
		}
	}
	httpc := &http.Client{Transport: &http.Transport{TLSClientConfig: insecureTLS()}}
	req, _ := http.NewRequest("POST", url+"/graph/v1.0/drives", strings.NewReader(`{"name":"fstest project"}`))
	req.SetBasicAuth(user, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d struct{ ID string }
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil || d.ID == "" {
		t.Fatalf("creating project space: HTTP %d %v", resp.StatusCode, err)
	}
	return d.ID
}

func TestConformanceRealDrives(t *testing.T) {
	url, user, token := realEnv(t)
	project := ensureProject(t, url, user, token)
	cl := openClient(t, url, user, token, insecure)
	ds, err := cl.Drives(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var personal string
	for _, d := range ds {
		kinds = append(kinds, string(d.Kind))
		if d.Kind == files.DrivePersonal {
			personal = d.ID
		}
	}
	if personal == "" || !strings.Contains(strings.Join(kinds, ","), "shared") {
		t.Fatalf("expected personal and shared drives, got %v", kinds)
	}
	for _, d := range ds {
		if d.Kind == files.DriveShared {
			fs, err := cl.OpenDrive(ctx, d.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fs.List(ctx, "/"); err != nil {
				t.Errorf("listing the shares space: %v", err)
			}
			if err := fs.Mkdir(ctx, "/x"); !errors.Is(err, odraz.ErrPermission) {
				t.Errorf("mkdir on shares: %v", err)
			}
		}
	}
	for name, id := range map[string]string{"personal": personal, "project": project} {
		id := id
		t.Run(name, func(t *testing.T) {
			fstest.Run(t, func(t *testing.T) files.FS {
				fs, err := openClient(t, url, user, token, insecure).OpenDrive(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				return fs
			})
		})
	}
}

// OIDC: the account's tokens are refreshed through the provider discovered from the server URL.
func TestOIDCRefreshWiring(t *testing.T) {
	var current atomic.Value
	current.Store("access-1")
	var grants []string
	var mu sync.Mutex
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/webfinger":
			io.WriteString(w, `{"links":[{"rel":"http://openid.net/specs/connect/1.0/issuer","href":"`+srv.URL+`"}]}`)
		case "/.well-known/openid-configuration":
			io.WriteString(w, `{"issuer":"`+srv.URL+`","authorization_endpoint":"`+srv.URL+`/a","token_endpoint":"`+srv.URL+`/token"}`)
		case "/token":
			r.ParseForm()
			mu.Lock()
			grants = append(grants, r.Form.Get("grant_type")+" client="+r.Form.Get("client_id")+" rt="+r.Form.Get("refresh_token"))
			mu.Unlock()
			current.Store("access-2")
			io.WriteString(w, `{"access_token":"access-2","refresh_token":"rt-2","expires_in":3600}`)
		case "/graph/v1.0/me/drives":
			if r.Header.Get("Authorization") != "Bearer "+current.Load().(string) {
				w.WriteHeader(401)
				return
			}
			io.WriteString(w, drivesJSON)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	st := accounts.NewMemoryStore()
	// Expired access token + refresh token, as saved by oidc.Save after a login.
	st.Put(ctx, "oc", accounts.SecretAccessToken, []byte("access-0"))
	st.Put(ctx, "oc", accounts.SecretRefreshToken, []byte("rt-1"))
	st.Put(ctx, "oc", accounts.SecretExpiry, []byte(time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)))
	acc := accounts.Account{ID: "oc", Adapter: "opencloud", Config: map[string]string{"url": srv.URL, accounts.ConfigOIDCClientID: "odraz-app"}}
	h, err := accounts.Open(ctx, acc, st)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := files.ForAccount(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	ds, err := cl.Drives(ctx)
	if err != nil || len(ds) != 4 {
		t.Fatalf("drives = %v, %v", ds, err)
	}
	if len(grants) != 1 || grants[0] != "refresh_token client=odraz-app rt=rt-1" {
		t.Errorf("token requests = %v", grants)
	}
	if v, _ := st.Get(ctx, "oc", accounts.SecretRefreshToken); string(v) != "rt-2" {
		t.Errorf("rotated refresh token not saved: %q", v)
	}
	// Without the client ID the account is not an OIDC account: no refresh, ErrAuth.
	delete(acc.Config, accounts.ConfigOIDCClientID)
	h, _ = accounts.Open(ctx, acc, st)
	st.Put(ctx, "oc", accounts.SecretAccessToken, []byte("stale"))
	cl, _ = files.ForAccount(ctx, h)
	if _, err := cl.Drives(ctx); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("non-OIDC account with a bad token: %v", err)
	}
}

func TestOIDCMethodDeclared(t *testing.T) {
	a, _ := accounts.Lookup("opencloud")
	var kinds []string
	for _, m := range a.AuthMethods {
		kinds = append(kinds, m.ID+":"+string(m.Kind))
	}
	if strings.Join(kinds, ",") != "oidc:browser,app-token:form" {
		t.Errorf("auth methods = %v", kinds)
	}
}

// OpenCloud renders thumbnails for a WebDAV GET that carries ?preview=1 and a size, and answers 404 for a file
// type it cannot render.
func TestPreview(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/graph/v1.0/me/drives":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"value":[{"id":"p1","name":"Me","driveType":"personal","root":{"webDavUrl":"https://public.example/dav/spaces/p1"}}]}`)
		case r.Method == "GET" && r.URL.Path == "/dav/spaces/p1/pic.png" && r.URL.Query().Get("preview") == "1":
			gotQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "image/png")
			io.WriteString(w, "PNGDATA")
		case r.Method == "GET" && r.URL.Path == "/dav/spaces/p1/odd.bin":
			w.Header().Set("Content-Type", "application/octet-stream") // a server that ignores ?preview and sends the file
			io.WriteString(w, "raw")
		case r.Method == "GET" && r.URL.Path == "/dav/spaces/p1/dir" && r.URL.Query().Get("preview") == "1":
			w.WriteHeader(400) // what real OpenCloud answers for a folder
		case r.Method == "GET" && r.URL.Query().Get("preview") == "1":
			w.WriteHeader(404) // no preview for this type, or a missing path
		case r.Method == "PROPFIND":
			res := map[string]string{
				"/dav/spaces/p1/note.txt": `<resourcetype/>`, "/dav/spaces/p1/odd.bin": `<resourcetype/>`,
				"/dav/spaces/p1/dir/": `<resourcetype><collection/></resourcetype>`, "/dav/spaces/p1/dir": `<resourcetype><collection/></resourcetype>`,
			}
			rt, ok := res[r.URL.Path]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(207)
			fmt.Fprintf(w, `<multistatus xmlns="DAV:"><response><href>%s</href><propstat><prop>%s</prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`, r.URL.Path, rt)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	fs, err := openClient(t, srv.URL, "u", "tok", nil).OpenDrive(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	rc, ct, err := files.Preview(ctx, fs, "/pic.png", 64, 48)
	if err != nil || ct != "image/png" {
		t.Fatalf("Preview = %v %q", err, ct)
	}
	if b, _ := io.ReadAll(rc); string(b) != "PNGDATA" {
		t.Errorf("body = %q", b)
	}
	rc.Close()
	for _, w := range []string{"preview=1", "x=64", "y=48", "scalingup=0"} {
		if !strings.Contains(gotQuery, w) {
			t.Errorf("query %q lacks %q", gotQuery, w)
		}
	}
	for path, want := range map[string]error{"/note.txt": odraz.ErrUnsupported, "/odd.bin": odraz.ErrUnsupported, "/dir": odraz.ErrInvalidPath, "/missing.png": odraz.ErrNotFound} {
		if _, _, err := files.Preview(ctx, fs, path, 64, 64); !errors.Is(err, want) {
			t.Errorf("Preview(%s) = %v, want %v", path, err, want)
		}
	}
	// A read-only drive still serves thumbnails.
	if _, ok := files.ReadOnly(fs).(files.Previewer); !ok {
		t.Error("ReadOnly lost the Previewer")
	}
}

// Real server: needs ODRAZ_TEST_OPENCLOUD_* as for the other real tests. Uploads a generated PNG, asks for a
// small thumbnail, and checks it is a real, smaller PNG; a binary file has none (text files do get one).
func TestRealPreview(t *testing.T) {
	url, user, token := realEnv(t)
	cl := openClient(t, url, user, token, insecure)
	ds, err := cl.Drives(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var personal string
	for _, d := range ds {
		if d.Kind == files.DrivePersonal {
			personal = d.ID
		}
	}
	fs, err := cl.OpenDrive(ctx, personal)
	if err != nil {
		t.Fatal(err)
	}
	dir := fmt.Sprintf("/preview-test-%d", time.Now().UnixNano())
	defer fs.Delete(ctx, dir, true)
	img := image.NewRGBA(image.Rect(0, 0, 640, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 640; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := files.Upload(ctx, fs, dir+"/pic.png", bytes.NewReader(buf.Bytes()), files.WriteOptions{Size: int64(buf.Len())}); err != nil {
		t.Fatal(err)
	}
	if err := files.Upload(ctx, fs, dir+"/blob.bin", bytes.NewReader([]byte{0, 1, 2, 3, 0xff, 0xfe}), files.WriteOptions{Size: 6}); err != nil {
		t.Fatal(err)
	}
	rc, ct, err := files.Preview(ctx, fs, dir+"/pic.png", 64, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || !strings.HasPrefix(ct, "image/") {
		t.Fatalf("not an image: %v %q", err, ct)
	}
	if cfg.Width > 64 || cfg.Height > 64 || len(data) >= buf.Len() {
		t.Errorf("thumbnail %dx%d, %d bytes (original %d bytes)", cfg.Width, cfg.Height, len(data), buf.Len())
	}
	if _, _, err := files.Preview(ctx, fs, dir+"/blob.bin", 64, 64); !errors.Is(err, odraz.ErrUnsupported) {
		t.Errorf("binary file: %v, want ErrUnsupported", err)
	}
	if _, _, err := files.Preview(ctx, fs, dir, 64, 64); !errors.Is(err, odraz.ErrInvalidPath) {
		t.Errorf("folder: %v, want ErrInvalidPath", err)
	}
}

func TestAvatar(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "":
			w.WriteHeader(401)
			return
		}
		user, _, _ := r.BasicAuth()
		switch {
		case r.URL.Path == "/graph/v1.0/me/photo/$value" && user == "withphoto":
			w.Header().Set("Content-Type", "image/jpeg")
			io.WriteString(w, "JPEGDATA")
		case r.URL.Path == "/graph/v1.0/me/photo/$value" && user == "html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, "<html>login</html>")
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(404) // what real OpenCloud answers for a user without a photo
			io.WriteString(w, `{"error":{"code":"generalException","message":"failed to get photo"}}`)
		}
	}))
	defer srv.Close()
	avatar := func(user string) (string, string, error) {
		st := accounts.NewMemoryStore()
		st.Put(ctx, "oc", accounts.SecretPassword, []byte("tok"))
		h, err := accounts.Open(ctx, accounts.Account{ID: "oc", Adapter: "opencloud", Config: map[string]string{"url": srv.URL, "username": user}}, st)
		if err != nil {
			t.Fatal(err)
		}
		rc, ct, err := h.Avatar(ctx, 64)
		if err != nil {
			return "", "", err
		}
		defer rc.Close()
		b, _ := io.ReadAll(rc)
		return string(b), ct, nil
	}
	if b, ct, err := avatar("withphoto"); err != nil || b != "JPEGDATA" || ct != "image/jpeg" {
		t.Errorf("with a photo: %q %q %v", b, ct, err)
	}
	if _, _, err := avatar("nophoto"); !errors.Is(err, odraz.ErrNotFound) {
		t.Errorf("without a photo: %v, want ErrNotFound", err)
	}
	if _, _, err := avatar("html"); !errors.Is(err, odraz.ErrNotFound) {
		t.Errorf("a non-image answer: %v, want ErrNotFound", err)
	}
}

// Real server: most users have no photo, so either an image or ErrNotFound is right.
func TestRealAvatar(t *testing.T) {
	url, user, token := realEnv(t)
	st := accounts.NewMemoryStore()
	st.Put(ctx, "oc", accounts.SecretPassword, []byte(token))
	h, err := accounts.Open(ctx, accounts.Account{ID: "oc", Adapter: "opencloud", Config: map[string]string{"url": url, "username": user, accounts.ConfigInsecure: "true"}}, st)
	if err != nil {
		t.Fatal(err)
	}
	rc, ct, err := h.Avatar(ctx, 64)
	if errors.Is(err, odraz.ErrNotFound) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if b, _ := io.ReadAll(rc); !strings.HasPrefix(ct, "image/") || len(b) == 0 {
		t.Errorf("avatar: %q, %d bytes", ct, len(b))
	}
}
