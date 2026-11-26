package s3

import (
	"context"
	"fmt"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
)

// Purge is not implemented yet.
func (f *fs) Purge(_ context.Context, p string) error {
	return fmt.Errorf("s3: %s: purge: %w", p, odraz.ErrUnsupported)
}
