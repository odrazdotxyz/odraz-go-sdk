// Package accounts holds the account model, credentials, secret storage and
// the adapter registry shared by all domains.
package accounts

// Account describes a configured connection to a self-hosted cloud.
// It never contains secrets; those live in a SecretStore keyed by ID.
type Account struct {
	ID      string
	Label   string
	Adapter string            // registered adapter name, e.g. "webdav"
	Config  map[string]string // non-secret settings: url, header.<Name>, ...
}
