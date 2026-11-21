package odraz_test

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
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
