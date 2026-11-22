package nextcloud_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
	"github.com/odrazdotxyz/odraz-go-sdk/files/fstest"
	_ "github.com/odrazdotxyz/odraz-go-sdk/vendors/nextcloud"
)

var ctx = context.Background()

func account(url, user string) accounts.Account {
	return accounts.Account{ID: "nc", Label: "NC", Adapter: "nextcloud", Config: map[string]string{"url": url, "username": user}}
}

func openClient(t *testing.T, url, user, pass string) files.Client {
	t.Helper()
	st := accounts.NewMemoryStore()
	st.Put(ctx, "nc", accounts.SecretPassword, []byte(pass))
	h, err := accounts.Open(ctx, account(url, user), st)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := files.ForAccount(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

func validate(url, user, pass string) error {
	st := accounts.NewMemoryStore()
	st.Put(ctx, "nc", accounts.SecretPassword, []byte(pass))
	return accounts.Validate(ctx, account(url, user), st)
}

func entry(href string, dir bool, mount, perms string, used, avail int64) string {
	rt := "<resourcetype/>"
	if dir {
		rt = "<resourcetype><collection/></resourcetype>"
	}
	return fmt.Sprintf(`<response><href>%s</href><propstat><prop>%s<nc:mount-type>%s</nc:mount-type><oc:permissions>%s</oc:permissions>`+
		`<quota-used-bytes>%d</quota-used-bytes><quota-available-bytes>%d</quota-available-bytes></prop><status>HTTP/1.1 200 OK</status></propstat></response>`,
		href, rt, mount, perms, used, avail)
}

// fakeNextcloud answers the OCS "who am I" call and one PROPFIND listing: the user's root
// (unlimited quota), a normal folder, a writable team folder and a read-only one.
func fakeNextcloud(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, p, _ := r.BasicAuth(); p != "pw" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == "/ocs/v2.php/cloud/user":
			if r.Header.Get("OCS-APIRequest") != "true" {
				w.WriteHeader(412)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"ocs":{"data":{"id":"a.li@ce","displayname":"Alice"}}}`)
		case r.Method == "PROPFIND" && r.URL.Path == "/remote.php/dav/files/a.li@ce/":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(207)
			io.WriteString(w, `<multistatus xmlns="DAV:" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">`+
				entry("/remote.php/dav/files/a.li@ce/", true, "", "RGDNVCK", 100, -3)+
				entry("/remote.php/dav/files/a.li@ce/Photos/", true, "", "RGDNVCK", 5, -3)+
				entry("/remote.php/dav/files/a.li@ce/Team%20Docs/", true, "group", "RMGDNVCK", 10, 990)+
				entry("/remote.php/dav/files/a.li@ce/Handbook/", true, "group", "MG", 0, 500)+
				`</multistatus>`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDrivesMapTeamFolders(t *testing.T) {
	srv := fakeNextcloud(t)
	ds, err := openClient(t, srv.URL, "alice", "pw").Drives(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []files.Drive{
		{ID: "personal", Name: "Alice", Kind: files.DrivePersonal, Quota: &files.Quota{Used: 100, Total: -1}},
		{ID: "team:Team Docs", Name: "Team Docs", Kind: files.DriveProject, Quota: &files.Quota{Used: 10, Total: 1000}},
		{ID: "team:Handbook", Name: "Handbook", Kind: files.DriveProject, Quota: &files.Quota{Used: 0, Total: 500}, ReadOnly: true},
	}
	if len(ds) != len(want) {
		t.Fatalf("got %d drives: %+v", len(ds), ds)
	}
	for i, w := range want {
		g := ds[i]
		if g.ID != w.ID || g.Name != w.Name || g.Kind != w.Kind || g.ReadOnly != w.ReadOnly || fmt.Sprint(g.Quota) != fmt.Sprint(w.Quota) {
			t.Errorf("drive %d = %+v quota=%v, want %+v quota=%v", i, g, g.Quota, w, w.Quota)
		}
	}
}

// The team folder's name has a space and the user id has "@" and ".": each must be escaped exactly once.
func TestOpenDriveEscapesPathOnce(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ocs/v2.php/cloud/user" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"ocs":{"data":{"id":"a.li@ce","displayname":"Alice"}}}`)
			return
		}
		if r.Method == "PROPFIND" && r.URL.EscapedPath() == "/remote.php/dav/files/a.li@ce/" {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(207)
			io.WriteString(w, `<multistatus xmlns="DAV:" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">`+
				entry("/remote.php/dav/files/a.li@ce/", true, "", "RGDNVCK", 1, -3)+
				entry("/remote.php/dav/files/a.li@ce/Team%20Docs/", true, "group", "RMGDNVCK", 1, -3)+`</multistatus>`)
			return
		}
		got = r.Method + " " + r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(207)
		io.WriteString(w, `<multistatus xmlns="DAV:"><response><href>/remote.php/dav/files/a.li@ce/Team%20Docs/</href><propstat><prop><resourcetype><collection/></resourcetype></prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`)
	}))
	defer srv.Close()
	fs, err := openClient(t, srv.URL, "alice", "pw").OpenDrive(ctx, "team:Team Docs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	if want := "PROPFIND /remote.php/dav/files/a.li@ce/Team%20Docs/"; got != want {
		t.Errorf("request = %q, want %q", got, want)
	}
}

func TestReadOnlyTeamFolder(t *testing.T) {
	srv := fakeNextcloud(t)
	cl := openClient(t, srv.URL, "alice", "pw")
	fs, err := cl.OpenDrive(ctx, "team:Handbook")
	if err != nil {
		t.Fatal(err)
	}
	if !fs.Capabilities().ReadOnly {
		t.Error("read-only team folder must report ReadOnly")
	}
	if err := fs.Mkdir(ctx, "/x"); !errors.Is(err, odraz.ErrPermission) {
		t.Errorf("mkdir: %v", err)
	}
}

func TestOpenDriveUnknown(t *testing.T) {
	srv := fakeNextcloud(t)
	cl := openClient(t, srv.URL, "alice", "pw")
	for _, id := range []string{"team:Nope", "team:", "team:a/b", "other", "Photos"} {
		if _, err := cl.OpenDrive(ctx, id); !errors.Is(err, odraz.ErrNotFound) {
			t.Errorf("OpenDrive(%q) = %v, want ErrNotFound", id, err)
		}
	}
}

func TestValidate(t *testing.T) {
	srv := fakeNextcloud(t)
	if err := validate(srv.URL, "alice", "pw"); err != nil {
		t.Errorf("valid: %v", err)
	}
	if err := validate(srv.URL, "alice", "bad"); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("bad password: %v", err)
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"hello":"not nextcloud"}`)
	}))
	defer other.Close()
	if err := validate(other.URL, "alice", "pw"); !errors.Is(err, odraz.ErrNotFound) {
		t.Errorf("not a Nextcloud server: %v", err)
	}
}

// Real server: testenv/nextcloud/seed.sh, then
//
//	ODRAZ_TEST_NEXTCLOUD_URL=http://localhost:8082 go test ./vendors/nextcloud
func realURL(t *testing.T) string {
	u := os.Getenv("ODRAZ_TEST_NEXTCLOUD_URL")
	if u == "" {
		t.Skip("ODRAZ_TEST_NEXTCLOUD_URL not set")
	}
	return u
}

func TestRealValidate(t *testing.T) {
	url := realURL(t)
	if err := validate(url, "alice", "alice-pass-123"); err != nil {
		t.Errorf("valid: %v", err)
	}
	if err := validate(url, "alice", "wrong"); !errors.Is(err, odraz.ErrAuth) {
		t.Errorf("bad password: %v", err)
	}
}

func TestRealTeamFolders(t *testing.T) {
	url := realURL(t)
	find := func(user, pass string) map[string]files.Drive {
		ds, err := openClient(t, url, user, pass).Drives(ctx)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]files.Drive{}
		for _, d := range ds {
			m[d.ID] = d
		}
		return m
	}
	alice := find("alice", "alice-pass-123")
	if d := alice["personal"]; d.Kind != files.DrivePersonal || d.ReadOnly {
		t.Errorf("alice personal = %+v", d)
	}
	td, ok := alice["team:Team Docs"]
	if !ok || td.Kind != files.DriveProject || td.ReadOnly || td.Quota == nil || td.Quota.Total != 64<<20 {
		t.Errorf("alice team folder = %+v (quota %v), want writable project drive with a 64 MiB quota", td, td.Quota)
	}
	if d := find("bob", "bob-pass-123")["team:Team Docs"]; d.Kind != files.DriveProject || !d.ReadOnly {
		t.Errorf("bob (viewers group) must see it read-only, got %+v", d)
	}
}

func TestRealReadOnlyEnforced(t *testing.T) {
	url := realURL(t)
	fs, err := openClient(t, url, "bob", "bob-pass-123").OpenDrive(ctx, "team:Team Docs")
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Mkdir(ctx, "/nope"); !errors.Is(err, odraz.ErrPermission) {
		t.Errorf("mkdir in a read-only team folder: %v", err)
	}
}

func TestConformanceReal(t *testing.T) {
	url := realURL(t)
	for name, id := range map[string]string{"personal": "personal", "team folder": "team:Team Docs"} {
		id := id
		t.Run(name, func(t *testing.T) {
			fstest.Run(t, func(t *testing.T) files.FS {
				fs, err := openClient(t, url, "alice", "alice-pass-123").OpenDrive(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				return fs
			})
		})
	}
}
