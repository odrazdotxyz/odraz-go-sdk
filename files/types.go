// Package files is the protocol-neutral files domain: drives, a filesystem
// interface, capabilities and helpers. Adapters implement Client.
package files

import (
	"context"
	"io"
	"time"
)

// DriveKind classifies a drive.
type DriveKind string

const (
	DrivePersonal DriveKind = "personal"
	DriveProject  DriveKind = "project"
	DriveShared   DriveKind = "shared"
	DriveOther    DriveKind = "other"
)

// Quota is optional storage usage; fields are -1 when unknown.
type Quota struct{ Used, Total int64 }

// Drive is a top-level storage root.
type Drive struct {
	ID    string
	Name  string
	Kind  DriveKind
	Quota *Quota // nil if the backend reports none
	// ReadOnly drives can be browsed and read but not modified (for example
	// OpenCloud's virtual Shares space). The zero value means writable.
	ReadOnly bool
}

// Entry describes a file or folder. Path is cleaned and drive-relative.
type Entry struct {
	Path     string
	Name     string
	IsDir    bool
	Size     int64
	ModTime  time.Time
	ETag     string
	MimeType string
}

// Capabilities report what a backend can do natively.
type Capabilities struct {
	AtomicMove      bool // Move is a single atomic server operation
	RealDirectories bool // empty folders exist (false for object stores)
	RangeRead       bool // Read honours offset/length natively
	ServerSideCopy  bool // server can copy without download (files.Copier)
	ReadOnly        bool // every mutating call fails with ErrPermission
}

// WriteOptions control FS.Write.
type WriteOptions struct {
	Size      int64 // content length, -1 if unknown
	Overwrite bool  // default false: existing target gives ErrExists
	// Progress, if set, is called with bytes sent so far and Size.
	Progress func(done, total int64)
}

// FS is a filesystem rooted at a drive. All paths are drive-relative and
// normalized with CleanPath; use Wrap to enforce this for an adapter.
type FS interface {
	Stat(ctx context.Context, path string) (Entry, error)
	// List returns direct children only.
	List(ctx context.Context, path string) ([]Entry, error)
	// Read returns length bytes from offset; length < 0 reads to the end.
	Read(ctx context.Context, path string, offset, length int64) (io.ReadCloser, error)
	Write(ctx context.Context, path string, r io.Reader, opts WriteOptions) error
	// Mkdir creates the folder and missing parents; no error if it exists
	// as a folder. ErrExists if a file occupies the path.
	Mkdir(ctx context.Context, path string) error
	// Move renames/moves a file or folder. Without overwrite, an existing
	// target gives ErrExists.
	Move(ctx context.Context, from, to string, overwrite bool) error
	// Delete removes a file or folder. A non-empty folder without recursive
	// gives ErrNotEmpty.
	Delete(ctx context.Context, path string, recursive bool) error
	Capabilities() Capabilities
}

// Client is the files domain of an account.
type Client interface {
	Drives(ctx context.Context) ([]Drive, error)
	OpenDrive(ctx context.Context, driveID string) (FS, error)
}

// Optional interfaces, detected by type assertion on FS. Copier and
// Previewer are implemented by some adapters; EditorOpener is reserved.
type (
	// Copier is implemented by backends with a native copy (WebDAV COPY).
	// Use the Copy helper, which validates arguments and falls back to
	// read+write when the FS does not implement it.
	Copier interface {
		Copy(ctx context.Context, from, to string, overwrite bool) error
	}
	// Previewer returns a thumbnail/preview image rendered by the server: the
	// body, its content type (an image/* type) and an error. Use the Preview
	// helper, which validates the size and returns ErrUnsupported when the FS
	// (or the file type) has no preview.
	Previewer interface {
		Preview(ctx context.Context, path string, width, height int) (io.ReadCloser, string, error)
	}
	// EditorOpener returns a URL to open the file in a web editor.
	EditorOpener interface {
		EditorURL(ctx context.Context, path string) (string, error)
	}
)
