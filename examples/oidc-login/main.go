// Command oidc-login logs in to an OpenCloud server with OIDC in your browser,
// lists the drives, then forces a token refresh. Use it to check a real
// server and identity provider (built-in IdP, Pocket ID, ...) once the admin
// has registered your client.
//
//	go run ./examples/oidc-login -server https://cloud.example.org -client-id odraz-app [-insecure]
//
// The redirect URI is a loopback address on a random port
// (http://127.0.0.1:<port>/callback). If your provider needs the exact URI
// registered, pass -redirect http://127.0.0.1:PORT/callback and the fixed PORT
// is used.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts/oidc"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
	_ "github.com/odrazdotxyz/odraz-go-sdk/vendors/opencloud"
)

func main() {
	server := flag.String("server", "", "OpenCloud server URL")
	clientID := flag.String("client-id", "", "OIDC client ID registered for your app")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification (self-signed servers)")
	flag.Parse()
	if *server == "" || *clientID == "" {
		flag.Usage()
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := run(ctx, *server, *clientID, *insecure); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, server, clientID string, insecure bool) error {
	store := accounts.NewMemoryStore() // a real app uses its keychain
	acc := accounts.Account{ID: "demo", Label: "demo", Adapter: "opencloud", Config: map[string]string{"url": server}}
	if insecure {
		acc.Config[accounts.ConfigInsecure] = "true"
	}
	h, err := accounts.Open(ctx, acc, store)
	if err != nil {
		return err
	}
	hc := oidc.HTTPClient(h.Env)

	lb, err := oidc.NewLoopback()
	if err != nil {
		return err
	}
	defer lb.Close()
	sess, err := oidc.Begin(ctx, hc, server, oidc.Config{ClientID: clientID, RedirectURI: lb.RedirectURI()})
	if err != nil {
		return fmt.Errorf("discovery/begin: %w", err)
	}
	fmt.Println("Open this URL in your browser and sign in:\n\n  " + sess.AuthorizationURL + "\n")
	callback, err := lb.Wait(ctx)
	if err != nil {
		return err
	}
	tokens, err := sess.Complete(ctx, callback)
	if err != nil {
		return fmt.Errorf("completing login: %w", err)
	}
	fmt.Printf("logged in; access token expires %s, refresh token: %v\n", tokens.Expiry.Format(time.RFC3339), tokens.RefreshToken != "")
	if err := oidc.Save(ctx, store, acc.ID, tokens); err != nil {
		return err
	}

	// Reopen the account the way an app does after a restart: client ID in Config.
	acc.Config[accounts.ConfigOIDCClientID] = clientID
	if h, err = accounts.Open(ctx, acc, store); err != nil {
		return err
	}
	cl, err := files.ForAccount(ctx, h)
	if err != nil {
		return err
	}
	drives, err := cl.Drives(ctx)
	if err != nil {
		return fmt.Errorf("listing drives with the access token: %w", err)
	}
	for _, d := range drives {
		fmt.Printf("  drive %-10s %-20s read-only=%v\n", d.Kind, d.Name, d.ReadOnly)
	}

	if tokens.RefreshToken == "" {
		fmt.Println("the provider issued no refresh token (is offline_access allowed for this client?)")
		return nil
	}
	refreshed, err := oidc.RefreshFunc(hc, server, clientID)(ctx, accounts.Credentials{Kind: accounts.CredBearer, RefreshToken: tokens.RefreshToken})
	if err != nil {
		return fmt.Errorf("refreshing: %w", err)
	}
	fmt.Printf("refresh works; new token expires %s\n", refreshed.Expiry.Format(time.RFC3339))
	return nil
}
