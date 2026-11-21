package opencloud_test

import "crypto/tls"

func insecureTLS() *tls.Config { return &tls.Config{InsecureSkipVerify: true} } //nolint:gosec // test against self-signed container
