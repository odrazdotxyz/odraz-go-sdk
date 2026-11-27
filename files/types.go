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
	// Metadata is stored with the object (keys are case-insensitive).
	// Backends without object metadata ignore it.
	Metadata map[string]string
	// SHA256, if set, is the expected SHA-256 of the content, in hex. A backend
	// that supports it checks the data as it streams, stores nothing on a
	// mismatch (ErrChecksum) and may keep the value, so that FS's Hasher returns
	// it without reading. Only backends that implement Hasher verify it; others
	// ignore it, so do not rely on it with those.
	SHA256 string
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

// Check compares what was written with what the store reports.
type Check interface {
	// Write takes the content, in order: exactly the bytes given to FS.Write.
	io.Writer
	// Verify returns nil if the file at path holds them, ErrChecksum if it
	// does not and ErrUnsupported if the store cannot tell without the file
	// being read.
	Verify(ctx context.Context, path string) error
}

// Optional interfaces, detected by type assertion on FS. Copier, Previewer,
// Hasher, ObjectStore and ConditionalDeleter are implemented by some adapters;
// EditorOpener is reserved.
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
	// Hasher returns the SHA-256 (lowercase hex) the backend stored for the
	// file, or ErrUnsupported when it has none. Use the SHA256 helper, which
	// falls back to reading the file.
	Hasher interface {
		SHA256(ctx context.Context, path string) (string, error)
	}
	// ObjectStore is implemented by backends that keep objects in a store of its
	// own kind (S3): they can confirm an upload from the server's own checksum
	// and remove a file with its old versions.
	ObjectStore interface {
		// NewCheck returns a Check for content of size bytes (-1 if unknown)
		// that Write stores.
		NewCheck(size int64) Check
		// Purge removes the file at path for good: every old version and delete
		// marker a store that keeps them holds. On a folder it removes the
		// folder's own marker and only if the folder holds nothing
		// (ErrNotEmpty). Nothing there is not an error.
		Purge(ctx context.Context, path string) error
	}
	// ConditionalDeleter removes a file only if it is still the one the caller
	// read. An FS made by Wrap or ReadOnly always has the method and returns
	// ErrUnsupported when the backend cannot promise that (some servers ignore
	// the condition and delete anyway, so those do not offer it).
	ConditionalDeleter interface {
		// DeleteIfMatch removes the file at path if its ETag (Entry.ETag) is
		// etag, and otherwise keeps it (ErrChanged). A folder, or an empty
		// etag, is ErrInvalidPath.
		DeleteIfMatch(ctx context.Context, path, etag string) error
	}
	// EditorOpener returns a URL to open the file in a web editor.
	EditorOpener interface {
		EditorURL(ctx context.Context, path string) (string, error)
	}
)
