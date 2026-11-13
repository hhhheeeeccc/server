package saml

import (
	"encoding/base64"
	"encoding/pem"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

type keycloakSAMLUser struct {
	id, username, email, password string
}

func (user keycloakSAMLUser) representation() map[string]any {
	return map[string]any{
		"id": user.id, "username": user.username, "enabled": true, "emailVerified": true,
		"email": user.email, "firstName": "Before", "lastName": "Keycloak",
		"credentials": []any{map[string]any{"type": "password", "value": user.password, "temporary": false}},
	}
}

func keycloakTestAdmin(t *testing.T) *keycloakAdmin {
	t.Helper()
	origin := os.Getenv("WORKSPACE_TEST_KEYCLOAK_URL")
	if origin == "" {
		t.Skip("use deployment/saml/run_keycloak_test.py for isolated real-IdP coverage")
	}
	parsed, err := url.Parse(origin)
	require.NoError(t, err)
	ip := net.ParseIP(parsed.Hostname())
	require.True(t, parsed.Scheme == "http" && ip != nil && ip.IsLoopback() && parsed.Port() != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.Path == "", "only a literal loopback Keycloak origin is allowed")
	return &keycloakAdmin{origin: origin, client: noRedirectClient()}
}

func configureKeycloakSAML(t *testing.T, f *fixture, admin *keycloakAdmin, realm, appOrigin string, user keycloakSAMLUser, sign, encrypt bool) {
	t.Helper()
	admin.authenticate(t)
	entityID := appOrigin + "/saml/entity"
	acs := appOrigin + "/login/sso/saml"
	block, _ := pem.Decode(f.certPEM)
	require.NotNil(t, block)
	certificate := base64.StdEncoding.EncodeToString(block.Bytes)
	mappers := []any{}
	for _, mapping := range [][2]string{{"id", "uid"}, {"username", "username"}, {"email", "mail"}, {"firstName", "givenName"}} {
		mappers = append(mappers, map[string]any{
			"name": mapping[1], "protocol": "saml", "protocolMapper": "saml-user-property-mapper",
			"config": map[string]string{"user.attribute": mapping[0], "attribute.name": mapping[1], "attribute.nameformat": "Basic"},
		})
	}
	admin.request(t, http.MethodPost, "/admin/realms", map[string]any{
		"realm": realm, "enabled": true, "sslRequired": "none",
		"clients": []any{map[string]any{
			"clientId": entityID, "protocol": "saml", "enabled": true,
			"redirectUris": []string{acs}, "protocolMappers": mappers,
			"attributes": map[string]string{
				"saml.server.signature": "true", "saml.assertion.signature": "true",
				"saml.client.signature": strconv.FormatBool(sign), "saml.signature.algorithm": "RSA_SHA256",
				"saml.force.post.binding": "true", "saml_assertion_consumer_url_post": acs,
				"saml.encrypt":                strconv.FormatBool(encrypt),
				"saml.encryption.certificate": certificate, "saml.signing.certificate": certificate,
			},
		}},
		"users": []any{user.representation()},
	}, http.StatusCreated)
	t.Cleanup(func() { admin.request(t, http.MethodDelete, "/admin/realms/"+realm, nil, http.StatusNoContent) })
	issuer := admin.origin + "/realms/" + realm
	idpCertificate := admin.signingCertificate(t, issuer)
	require.NoError(t, f.th.Server.Platform().SetConfigFile("test-keycloak-idp.pem", idpCertificate))
	f.th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.ServiceSettings.SiteURL = new(appOrigin)
		cfg.SamlSettings.AssertionConsumerServiceURL = &acs
		cfg.SamlSettings.IdpURL = new(issuer + "/protocol/saml")
		cfg.SamlSettings.IdpDescriptorURL = &issuer
		cfg.SamlSettings.IdpCertificateFile = new("test-keycloak-idp.pem")
		cfg.SamlSettings.ServiceProviderIdentifier = &entityID
		cfg.SamlSettings.SignRequest = &sign
		cfg.SamlSettings.Encrypt = &encrypt
	})
}
