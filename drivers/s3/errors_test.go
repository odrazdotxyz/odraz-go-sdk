package s3

import (
	"errors"
	"net/http"
	"testing"

	"github.com/minio/minio-go/v7"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
)

// The library's own refusal to send a request is not a server error.
func TestMapClientSideInvalidArgument(t *testing.T) {
	r := minio.ErrorResponse{StatusCode: http.StatusBadRequest, Code: "InvalidArgument", RequestID: "minio"}
	if err := mapErr(r); !errors.Is(err, odraz.ErrInvalidPath) || errors.Is(err, errServer) {
		t.Fatalf("client side: %v", err)
	}
	r.RequestID = "17F0"
	if err := mapErr(r); !errors.Is(err, errServer) || errors.Is(err, odraz.ErrInvalidPath) {
		t.Fatalf("server side: %v", err)
	}
}
