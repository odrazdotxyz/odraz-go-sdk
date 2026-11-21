package opencloud_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

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
