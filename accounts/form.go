package accounts

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Form key prefixes for open-ended extra headers (auth proxies).
const (
	FormHeaderPrefix       = "header."        // header.<Name>: stored in Config
	FormSecretHeaderPrefix = "secret_header." // secret_header.<Name>: stored in the SecretStore
)

// ValidationError names the form field that is missing or malformed.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("field %q: %s", e.Field, e.Reason) }

// SplitForm validates submitted form values against fields and separates
// them into non-secret config and secrets. Values for fields flagged Secret
// (and Type secret) go to secrets, everything else to config. Open-ended
// header.<Name> / secret_header.<Name> keys are handled as described on
// Config conventions. Unknown keys are rejected.
func SplitForm(fields []Field, values map[string]string) (config map[string]string, secrets map[string][]byte, err error) {
	config, secrets = map[string]string{}, map[string][]byte{}
	known := map[string]Field{}
	for _, f := range fields {
		known[f.Name] = f
	}
	var secretHeaders []string
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := values[k]
		switch {
		case strings.HasPrefix(k, FormHeaderPrefix) && len(k) > len(FormHeaderPrefix):
			config[ConfigHeaderPrefix+k[len(FormHeaderPrefix):]] = v
		case strings.HasPrefix(k, FormSecretHeaderPrefix) && len(k) > len(FormSecretHeaderPrefix):
			name := k[len(FormSecretHeaderPrefix):]
			secrets[secretHeaderPrefix+name] = []byte(v)
			secretHeaders = append(secretHeaders, name)
		default:
			if _, ok := known[k]; !ok {
				return nil, nil, &ValidationError{k, "unknown field"}
			}
		}
	}
	for _, f := range fields {
		v := strings.TrimSpace(values[f.Name])
		if f.Secret || f.Type == FieldSecret {
			v = values[f.Name] // never trim secrets
		}
		if v == "" {
			if f.Required {
				return nil, nil, &ValidationError{f.Name, "is required"}
			}
			continue
		}
		switch f.Type {
		case FieldURL:
			u, perr := url.Parse(v)
			if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return nil, nil, &ValidationError{f.Name, "must be an http(s) URL"}
			}
		case FieldNumber:
			if _, perr := strconv.ParseFloat(v, 64); perr != nil {
				return nil, nil, &ValidationError{f.Name, "must be a number"}
			}
		}
		if f.Secret || f.Type == FieldSecret {
			secrets[f.Name] = []byte(v)
		} else {
			config[f.Name] = v
		}
	}
	if len(secretHeaders) > 0 {
		config[ConfigSecretHeaders] = strings.Join(secretHeaders, ",")
	}
	return config, secrets, nil
}

// Configure applies submitted form values for the given auth method to an
// account: secrets are written to the store, the rest becomes acc.Config.
// The returned Account is ready to persist by the app.
func Configure(ctx context.Context, store SecretStore, acc Account, methodID string, values map[string]string) (Account, error) {
	a, ok := Lookup(acc.Adapter)
	if !ok {
		return acc, fmt.Errorf("accounts: adapter %q is not registered", acc.Adapter)
	}
	var method *AuthMethod
	for i := range a.AuthMethods {
		if a.AuthMethods[i].ID == methodID {
			method = &a.AuthMethods[i]
		}
	}
	if method == nil || method.Kind != AuthForm {
		return acc, fmt.Errorf("accounts: adapter %q has no form auth method %q", acc.Adapter, methodID)
	}
	fields := append(append([]Field{}, a.Fields...), method.Fields...)
	cfg, secrets, err := SplitForm(fields, values)
	if err != nil {
		return acc, err
	}
	for k, v := range secrets {
		if err := store.Put(ctx, acc.ID, k, v); err != nil {
			return acc, err
		}
	}
	acc.Config = cfg
	return acc, nil
}
