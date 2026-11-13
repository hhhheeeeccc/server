package office365

import (
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/mattermost/enterprise/oauth/profile"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
)

type Provider struct{ license func() *model.License }

var tenantName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)

var _ einterfaces.OAuthProvider = (*Provider)(nil)

func New(license func() *model.License) *Provider { return &Provider{license: license} }
func (p *Provider) enabled() bool {
	return p.license != nil && profile.Licensed(p.license(), model.ServiceOffice365)
}

func (p *Provider) GetUserFromJSON(rctx request.CTX, data io.Reader, _ *model.User, settings *model.SSOSettings) (*model.User, error) {
	if !p.enabled() {
		return nil, errors.New("Microsoft 365 OAuth requires an active licensed feature")
	}
	var graph struct {
		ID        string `json:"id"`
		Mail      string `json:"mail"`
		Principal string `json:"userPrincipalName"`
		Given     string `json:"givenName"`
		Family    string `json:"surname"`
		Display   string `json:"displayName"`
	}
	if err := profile.Decode(data, &graph); err != nil {
		return nil, err
	}
	email := graph.Mail
	if email == "" {
		email = graph.Principal
	}
	username := ""
	if settings != nil && model.SafeDereference(settings.UsePreferredUsername) {
		username = strings.SplitN(graph.Principal, "@", 2)[0]
	}
	return profile.User(rctx, model.ServiceOffice365, graph.ID, email, username, graph.Given, graph.Family, true)
}

func (p *Provider) GetSSOSettings(_ request.CTX, config *model.Config, service string) (*model.SSOSettings, error) {
	if !p.enabled() || config == nil || service != model.ServiceOffice365 {
		return nil, errors.New("Microsoft 365 OAuth is unavailable")
	}
	settings := config.Office365Settings.SSOSettings()
	tenant := strings.TrimSpace(model.SafeDereference(config.Office365Settings.DirectoryId))
	if tenant != "" {
		if !tenantName.MatchString(tenant) || len(tenant) > 253 || strings.Contains(tenant, "..") {
			return nil, errors.New("invalid Microsoft directory identifier")
		}
		// Bind both legs of the authorization-code exchange to the configured directory.
		for _, endpoint := range []**string{&settings.AuthEndpoint, &settings.TokenEndpoint} {
			u, err := url.Parse(model.SafeDereference(*endpoint))
			if err != nil {
				return nil, err
			}
			if strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), "login.microsoftonline.com") {
				parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
				if len(parts) < 2 {
					return nil, errors.New("invalid Microsoft OAuth endpoint")
				}
				if parts[0] != "common" && parts[0] != "organizations" && !strings.EqualFold(parts[0], tenant) {
					return nil, errors.New("Microsoft endpoint does not match the configured directory")
				}
				parts[0] = tenant
				u.Path = "/" + strings.Join(parts, "/")
				*endpoint = new(u.String())
			}
		}
	}
	return profile.Settings(settings)
}
func (p *Provider) GetUserFromIdToken(request.CTX, string) (*model.User, error) {
	if !p.enabled() {
		return nil, errors.New("Microsoft 365 OAuth is unavailable")
	}
	// The core obtains /me using the access token. Do not authenticate decoded JWT claims.
	return nil, nil
}
func (*Provider) IsSameUser(_ request.CTX, a, b *model.User) bool {
	return profile.SameUser(model.ServiceOffice365, a, b)
}
