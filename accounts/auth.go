package accounts

// FieldType is the input type of a form field.
type FieldType string

const (
	FieldText   FieldType = "text"
	FieldSecret FieldType = "secret"
	FieldURL    FieldType = "url"
	FieldNumber FieldType = "number"
)

// Field describes one input a UI must collect.
type Field struct {
	Name     string
	Label    string
	Type     FieldType
	Required bool
	Secret   bool // value goes to SecretStore, not Account.Config
}

// AuthKind is how a login is performed.
type AuthKind string

const (
	AuthForm    AuthKind = "form"    // UI collects Fields
	AuthBrowser AuthKind = "browser" // reserved for OIDC; not implemented in v1
)

// AuthMethod is one way to log in to an adapter.
type AuthMethod struct {
	ID     string
	Label  string
	Kind   AuthKind
	Fields []Field // for AuthForm
}
