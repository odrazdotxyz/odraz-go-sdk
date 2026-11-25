package odraz_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/drivers/s3" // registers the adapter
	"github.com/odrazdotxyz/odraz-go-sdk/files"

	_ "github.com/odrazdotxyz/odraz-go-sdk/drivers/webdav" // register the adapters you want
	_ "github.com/odrazdotxyz/odraz-go-sdk/vendors/opencloud"
)

// Example shows the whole flow: add an account, list drives, list, upload,
// download. It is compiled with the tests but not run (it needs a server).
func Example() {
	ctx := context.Background()

	// 1. The app supplies a SecretStore (Keychain, Keystore, OS keyring...).
	//    MemoryStore is for tests only.
	store := accounts.NewMemoryStore()

	// 2. Add an account from form values. Secrets go to the store, the rest
	//    comes back as Account.Config, which the app persists.
	acc, err := accounts.Configure(ctx, store,
		accounts.Account{ID: "home", Label: "Home cloud", Adapter: "opencloud"},
		"app-token", map[string]string{
			"url":      "https://cloud.example.org",
			"username": "alice",
			"password": "my app token",
		})
	if err != nil {
		panic(err)
	}

	// 3. Check it works. Errors are ErrAuth, ErrNotFound or ErrBlockedByProxy.
	if err := accounts.Validate(ctx, acc, store); err != nil {
		panic(err)
	}

	// 4. Open the account and get its files client.
	handle, err := accounts.Open(ctx, acc, store)
	if err != nil {
		panic(err)
	}
	client, err := files.ForAccount(ctx, handle) // ErrUnsupported if the adapter has no files
	if err != nil {
		panic(err)
	}

	// 5. List drives and open one.
	drives, err := client.Drives(ctx)
	if err != nil {
		panic(err)
	}
	fs, err := client.OpenDrive(ctx, drives[0].ID)
	if err != nil {
		panic(err)
	}

	// 6. List a folder, upload (creates parent folders), download.
	entries, _ := fs.List(ctx, "/")
	for _, e := range entries {
		fmt.Println(e.Name, e.IsDir, e.Size)
	}
	body := "hello"
	err = files.Upload(ctx, fs, "/notes/hello.txt", strings.NewReader(body),
		files.WriteOptions{Size: int64(len(body)), Overwrite: true})
	if err != nil {
		panic(err)
	}
	err = files.DownloadFile(ctx, fs, "/notes/hello.txt", os.TempDir()+"/hello.txt", files.DownloadOptions{})
	if err != nil {
		panic(err)
	}
}

// Example_s3Archive adds an S3 account, uploads a file that must not replace
// an existing one, and verifies it by reading it back. It is compiled with the
// tests but not run (it needs a server).
func Example_s3Archive() {
	ctx := context.Background()
	store := accounts.NewMemoryStore() // tests only; use a real SecretStore

	acc, err := accounts.Configure(ctx, store,
		accounts.Account{ID: "archive", Label: "Archive", Adapter: s3.Name},
		"access-key", map[string]string{
			"endpoint": "https://s3.example.org", // leave out for AWS
			"region":   "us-east-1",
			"bucket":   "photos",
			"prefix":   "archive",
			"username": "my access key id",
			"password": "my secret access key",
		})
	if err != nil {
		panic(err)
	}
	if err := accounts.Validate(ctx, acc, store); err != nil {
		panic(err)
	}
	handle, err := accounts.Open(ctx, acc, store)
	if err != nil {
		panic(err)
	}
	client, err := files.ForAccount(ctx, handle)
	if err != nil {
		panic(err)
	}
	fs, err := client.OpenDrive(ctx, s3.DriveID) // an S3 account has one drive
	if err != nil {
		panic(err)
	}

	f, err := os.Open("photo.jpg")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		panic(err)
	}

	// Hash the source while it is uploaded. Overwrite is false, so an
	// existing object gives ErrExists (see the drivers/s3 documentation for
	// the limits of that with large uploads).
	sent := sha256.New()
	err = files.Upload(ctx, fs, "/2026/photo.jpg", io.TeeReader(f, sent), files.WriteOptions{
		Size:     st.Size(),
		Metadata: map[string]string{"source": "camera"},
	})
	if err != nil {
		panic(err)
	}

	// files.SHA256 would return the hash recorded at upload. To know what is
	// actually stored, read the object back.
	rc, err := fs.Read(ctx, "/2026/photo.jpg", 0, -1)
	if err != nil {
		panic(err)
	}
	defer rc.Close()
	stored := sha256.New()
	if _, err := io.Copy(stored, rc); err != nil {
		panic(err)
	}
	fmt.Println("verified:", bytes.Equal(sent.Sum(nil), stored.Sum(nil)))
}
