package s3_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// Credentials and bucket every fake S3 server knows. The server creates
// the bucket fakeBucket itself, so tests can use it at once. gofakes3 does not
// authenticate requests, so newFakeS3 puts a handler in front of it that
// accepts only the access key id fakeAccessKey (any signature) and answers every
// other key with 403 InvalidAccessKeyId, like AWS. The secret is a long,
// recognisable string so tests can check it never appears in an error.
const (
	fakeAccessKey = "AKIATEST"
	fakeSecret    = "SECRETSECRETSECRET1234567890"
	fakeBucket    = "test"
)

func fakeHandler() http.Handler {
	backend := s3mem.New()
	if err := backend.CreateBucket(fakeBucket); err != nil {
		panic(err)
	}
	s3 := gofakes3.New(backend).Server()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "Credential="+fakeAccessKey+"/") {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>InvalidAccessKeyId</Code>` +
				`<Message>The AWS Access Key Id you provided does not exist in our records.</Message></Error>`))
			return
		}
		s3.ServeHTTP(w, r)
	})
}

// newFakeS3 starts an in-process S3 over plain HTTP and returns its endpoint
// ("http://127.0.0.1:port"). See fakeAccessKey, fakeSecret and fakeBucket.
func newFakeS3(t *testing.T) (endpoint string) {
	t.Helper()
	srv := httptest.NewServer(fakeHandler())
	t.Cleanup(srv.Close)
	return srv.URL
}

// newFakeS3TLS is newFakeS3 over HTTPS with a self-signed certificate (use
// insecure_skip_verify). It returns "https://127.0.0.1:port".
func newFakeS3TLS(t *testing.T) (endpoint string) {
	t.Helper()
	srv := httptest.NewTLSServer(fakeHandler())
	t.Cleanup(srv.Close)
	return srv.URL
}
