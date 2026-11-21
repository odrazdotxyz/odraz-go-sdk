package fstest

import (
	"testing"

	"github.com/odrazdotxyz/odraz-go-sdk/files"
	"github.com/odrazdotxyz/odraz-go-sdk/internal/memfs"
)

func TestMemFS(t *testing.T) { Run(t, func(*testing.T) files.FS { return memfs.New() }) }
