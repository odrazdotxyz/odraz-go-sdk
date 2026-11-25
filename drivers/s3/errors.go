package s3

import (
	"context"
	"errors"
	"net/http"

	"github.com/minio/minio-go/v7"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
)

// mapped is an error that carries an SDK sentinel and the client library's
// error. Its text is built here from the S3 error code only, never from the
// request or its credentials; the library error stays reachable through
// errors.Is and errors.As.
type mapped struct {
	msg      string
	sentinel error
	cause    error
}

func (e *mapped) Error() string   { return e.msg + ": " + e.sentinel.Error() }
func (e *mapped) Unwrap() []error { return []error{e.sentinel, e.cause} }

func newMapped(op, code string, sentinel, cause error) error {
	msg := "s3: " + op
	if code != "" {
		msg += " (" + code + ")"
	}
	return &mapped{msg: msg, sentinel: sentinel, cause: cause}
}

// mapErr translates an error of an S3 operation into the SDK's sentinels.
// Unknown errors are returned unchanged.
func mapErr(err error) error { return mapOp("request failed", err, false) }

// mapValidateErr is mapErr for Validate: a refused request there means the
// keys are wrong or lack access, so AccessDenied becomes ErrAuth.
func mapValidateErr(err error) error { return mapOp("cannot access bucket", err, true) }

func mapOp(op string, err error, validating bool) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	resp := minio.ToErrorResponse(err)
	if resp.Code == "" && resp.StatusCode == 0 {
		return err // not an S3 error: network, TLS, ...
	}
	// The client library refuses some requests before sending them and reports
	// that as InvalidArgument with itself as the request id.
	if resp.Code == "InvalidArgument" && resp.RequestID == "minio" {
		return newMapped("request rejected before it was sent (check names and metadata)", resp.Code, odraz.ErrInvalidPath, err)
	}
	switch resp.Code {
	case "InvalidAccessKeyId", "SignatureDoesNotMatch", "InvalidToken", "ExpiredToken", "AuthorizationHeaderMalformed":
		return newMapped(op, resp.Code, odraz.ErrAuth, err)
	case "AccessDenied", "AllAccessDisabled":
		if validating {
			return newMapped(op, resp.Code, odraz.ErrAuth, err)
		}
		return newMapped(op, resp.Code, odraz.ErrPermission, err)
	case "NoSuchKey", "NoSuchBucket", "NoSuchUpload", "NotFound":
		return newMapped(op, resp.Code, odraz.ErrNotFound, err)
	case "PreconditionFailed", "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
		return newMapped(op, resp.Code, odraz.ErrExists, err)
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return newMapped(op, resp.Code, odraz.ErrAuth, err)
	case http.StatusForbidden:
		if validating {
			return newMapped(op, resp.Code, odraz.ErrAuth, err)
		}
		return newMapped(op, resp.Code, odraz.ErrPermission, err)
	case http.StatusNotFound:
		return newMapped(op, resp.Code, odraz.ErrNotFound, err)
	case http.StatusPreconditionFailed:
		return newMapped(op, resp.Code, odraz.ErrExists, err)
	}
	return newMapped(op, resp.Code, errServer, err)
}

// errServer marks an S3 error with no better sentinel.
var errServer = errors.New("server error")
