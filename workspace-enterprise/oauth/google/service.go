package google

import (
	"errors"
	"io"

	"github.com/mattermost/enterprise/oauth/profile"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
)

type Provider struct{ license func() *model.License }

var _ einterfaces.OAuthProvider = (*Provider)(nil)

func New(license func() *model.License) *Provider { return &Provider{license: license} }
func (p *Provider) enabled() bool {
	return p.license != nil && profile.Licensed(p.license(), model.ServiceGoogle)
}

type fieldMetadata struct {
	Primary  bool `json:"primary"`
	Verified bool `json:"verified"`
}
type person struct {
	Metadata struct {
		Sources []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"sources"`
	} `json:"metadata"`
	Names []struct {
		Given    string        `json:"givenName"`
		Family   string        `json:"familyName"`
		Metadata fieldMetadata `json:"metadata"`
	} `json:"names"`
	Emails []struct {
		Value    string        `json:"value"`
		Metadata fieldMetadata `json:"metadata"`
	} `json:"emailAddresses"`
	Nicknames []struct {
		Value    string        `json:"value"`
		Metadata fieldMetadata `json:"metadata"`
	} `json:"nicknames"`
}

func (p *Provider) GetUserFromJSON(rctx request.CTX, data io.Reader, _ *model.User, _ *model.SSOSettings) (*model.User, error) {
	if !p.enabled() {
		return nil, errors.New("Google OAuth requires an active licensed feature")
	}
	var user person
	if err := profile.Decode(data, &user); err != nil {
		return nil, err
	}
	var subject, email, first, last, nickname string
	for _, source := range user.Metadata.Sources {
		if source.Type == "PROFILE" {
			if subject != "" && subject != source.ID {
				return nil, errors.New("ambiguous Google profile identifier")
			}
			subject = source.ID
		}
	}
	// resourceName can change after profile merging. Authentication uses the
	// immutable PROFILE source ID, and never a contact or an email identifier.
	for _, address := range user.Emails {
		if address.Metadata.Primary && address.Metadata.Verified {
			email = address.Value
		}
	}
	for _, name := range user.Names {
		if name.Metadata.Primary {
			first, last = name.Given, name.Family
		}
	}
	for _, name := range user.Nicknames {
		if name.Metadata.Primary {
			nickname = name.Value
		}
	}
	return profile.User(rctx, model.ServiceGoogle, subject, email, nickname, first, last, true)
}

func (p *Provider) GetSSOSettings(_ request.CTX, config *model.Config, service string) (*model.SSOSettings, error) {
	if !p.enabled() || config == nil || service != model.ServiceGoogle {
		return nil, errors.New("Google OAuth is unavailable")
	}
	settings := config.GoogleSettings
	return profile.Settings(&settings)
}

func (p *Provider) GetUserFromIdToken(request.CTX, string) (*model.User, error) {
	if !p.enabled() {
		return nil, errors.New("Google OAuth is unavailable")
	}
	// Identity comes exclusively from Google's authenticated People API response
	// fetched by the core with the exchanged access token; never from an unverified JWT.
	return nil, nil
}
func (*Provider) IsSameUser(_ request.CTX, a, b *model.User) bool {
	return profile.SameUser(model.ServiceGoogle, a, b)
}
