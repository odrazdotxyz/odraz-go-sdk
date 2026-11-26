// Package odraz is the root of the Odraz SDK. It holds the shared sentinel
// errors used by every domain package and adapter.
package odraz

import "errors"

// Sentinel errors. Adapters must wrap the underlying error so that both
// errors.Is(err, odraz.ErrX) and the original cause remain usable.
var (
	ErrNotFound       = errors.New("not found")
	ErrExists         = errors.New("already exists")
	ErrNotEmpty       = errors.New("directory not empty")
	ErrPermission     = errors.New("permission denied")
	ErrUnsupported    = errors.New("unsupported")
	ErrAuth           = errors.New("authentication failed")
	ErrBlockedByProxy = errors.New("request blocked by an authentication proxy")
	// ErrInvalidPath is returned for malformed paths or paths that escape
	// the drive root.
	ErrInvalidPath = errors.New("invalid path")
	// ErrChecksum means the data does not match the checksum the caller gave
	// (or that checksum is malformed): nothing was stored.
	ErrChecksum = errors.New("checksum mismatch")
)
