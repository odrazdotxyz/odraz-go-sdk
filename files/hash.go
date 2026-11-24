package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
)

// SHA256 returns the lowercase hex SHA-256 of the file at path. It asks the
// FS for a stored checksum when it implements Hasher; if that is missing
// (ErrUnsupported) or fails, it streams the file through crypto/sha256.
func SHA256(ctx context.Context, fs FS, path string) (string, error) {
	if h, ok := fs.(Hasher); ok {
		if sum, err := h.SHA256(ctx, path); err == nil {
			return sum, nil
		} else if !errors.Is(err, odraz.ErrUnsupported) && ctx.Err() != nil {
			return "", err
		}
	}
	r, err := fs.Read(ctx, path, 0, -1)
	if err != nil {
		return "", err
	}
	defer r.Close()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
