package ldap

import (
	"testing"

	ldapclient "github.com/mattermost/ldap"
	"github.com/stretchr/testify/require"
)

func TestLDAPDNKeyMatchesDirectoryEquality(t *testing.T) {
	dns := []string{
		"uid=one,dc=example,dc=test",
		"UID=one,DC=example,DC=test",
		"uid=One,dc=example,dc=test",
		"uid=one,dc=other,dc=test",
		"cn=alpha+ou=engineering,dc=example,dc=test",
		"OU=engineering+CN=alpha,DC=example,DC=test",
		"cn=alpha+cn=alpha+ou=engineering,dc=example,dc=test",
		"cn=alpha+ou=engineering+ou=engineering,dc=example,dc=test",
		`cn=last\2C first,dc=example,dc=test`,
		`cn=last\, first,dc=example,dc=test`,
		"cn=last,dc=example,dc=test",
		"uid=one,dc=test,dc=example",
	}
	parsed := make([]*ldapclient.DN, len(dns))
	for i, value := range dns {
		var err error
		parsed[i], err = ldapclient.ParseDN(value)
		require.NoError(t, err)
	}
	for i, left := range parsed {
		for j, right := range parsed {
			require.Equal(t, left.Equal(right), ldapDNKey(left) == ldapDNKey(right), "%q vs %q", dns[i], dns[j])
		}
	}
}
