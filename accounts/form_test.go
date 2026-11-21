package accounts

import (
	"context"
	"errors"
	"testing"
)

var testFields = []Field{
	{Name: "url", Type: FieldURL, Required: true},
	{Name: "username", Type: FieldText, Required: true},
	{Name: "password", Type: FieldSecret, Required: true, Secret: true},
	{Name: "port", Type: FieldNumber},
}

func TestSplitForm(t *testing.T) {
	cfg, sec, err := SplitForm(testFields, map[string]string{
		"url": " https://h/dav ", "username": "u", "password": " p ", "port": "8080",
		"header.CF-Access-Client-Id": "id", "secret_header.CF-Access-Client-Secret": "s3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg["url"] != "https://h/dav" || cfg["username"] != "u" || cfg["header.CF-Access-Client-Id"] != "id" ||
		cfg[ConfigSecretHeaders] != "CF-Access-Client-Secret" {
		t.Errorf("config = %v", cfg)
	}
	if _, leaked := cfg["password"]; leaked {
		t.Error("password leaked into config")
	}
	if string(sec["password"]) != " p " || string(sec["header.CF-Access-Client-Secret"]) != "s3" {
		t.Errorf("secrets = %v", sec)
	}
}

func TestSplitFormErrors(t *testing.T) {
	for name, vals := range map[string]map[string]string{
		"missing required": {"url": "https://h", "username": "u"},
		"bad url":          {"url": "ftp://h", "username": "u", "password": "p"},
		"bad number":       {"url": "https://h", "username": "u", "password": "p", "port": "x"},
		"unknown":          {"url": "https://h", "username": "u", "password": "p", "wat": "1"},
	} {
		_, _, err := SplitForm(testFields, vals)
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestConfigureEndToEnd(t *testing.T) {
	ctx := context.Background()
	Register(Adapter{Name: "form-test-adapter", Fields: []Field{{Name: "url", Type: FieldURL, Required: true}},
		AuthMethods: []AuthMethod{{ID: "basic", Kind: AuthForm, Fields: testFields[1:3]}}})
	s := NewMemoryStore()
	acc, err := Configure(ctx, s, Account{ID: "x", Adapter: "form-test-adapter"}, "basic",
		map[string]string{"url": "https://h", "username": "u", "password": "p", "secret_header.P-Access-Token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	h, err := Open(ctx, acc, s)
	if err != nil {
		t.Fatal(err)
	}
	if h.Env.Headers["P-Access-Token"] != "tok" {
		t.Errorf("secret header not resolved: %v", h.Env.Headers)
	}
	if c, _ := h.Env.Creds.Credentials(ctx); c.Username != "u" || c.Password != "p" || c.Kind != CredBasic {
		t.Errorf("creds = %+v", c)
	}
	if _, err := Configure(ctx, s, Account{ID: "x", Adapter: "form-test-adapter"}, "nope", nil); err == nil {
		t.Error("unknown method must fail")
	}
}
