package adminweb

import "testing"

// The console starts only with an identity configuration it can honour: a
// mode it does not have, or half of the directory's settings, is refused
// rather than quietly ignored.
func TestOnlyAnIdentityConfigurationTheConsoleCanHonourStarts(t *testing.T) {
	for name, o := range map[string]DatabaseOptions{
		"default":        {},
		"local":          {IdentityMode: IdentityLocal},
		"with directory": {IdentityMode: IdentityLocal, DirectoryURL: "https://tool.example.com/internal/directory/accounts", DirectoryToken: "t"},
	} {
		if err := o.checkIdentity(); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	for name, o := range map[string]DatabaseOptions{
		"oidc, not built yet": {IdentityMode: IdentityOIDC},
		"unknown mode":        {IdentityMode: "ldap"},
		"url without token":   {DirectoryURL: "https://tool.example.com/x"},
		"token without url":   {DirectoryToken: "t"},
	} {
		if err := o.checkIdentity(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
