// Package profile contains validation shared by the Google and Microsoft providers.
package profile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

const maxProfileBytes = 1 << 20

func Licensed(license *model.License, service string) bool {
	if license == nil || license.Features == nil {
		return false
	}
	switch service {
	case model.ServiceGoogle:
		return model.SafeDereference(license.Features.GoogleOAuth)
	case model.ServiceOffice365:
		return model.SafeDereference(license.Features.Office365OAuth)
	}
	return false
}

func Decode(data io.Reader, dest any) error {
	body, err := io.ReadAll(io.LimitReader(data, maxProfileBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxProfileBytes {
		return errors.New("OAuth profile response exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(dest); err != nil {
		return err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("invalid OAuth profile response")
	}
	return nil
}

func Settings(settings *model.SSOSettings) (*model.SSOSettings, error) {
	if !model.SafeDereference(settings.Enable) {
		return nil, errors.New("OAuth provider is disabled")
	}
	if strings.TrimSpace(model.SafeDereference(settings.Id)) == "" || model.SafeDereference(settings.Secret) == "" {
		return nil, errors.New("OAuth client credentials are not configured")
	}
	for _, endpoint := range []*string{settings.AuthEndpoint, settings.TokenEndpoint, settings.UserAPIEndpoint} {
		u, err := url.Parse(model.SafeDereference(endpoint))
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return nil, errors.New("OAuth endpoints must use HTTPS")
		}
	}
	return settings, nil
}

func User(rctx request.CTX, service, subject, email, username, first, last string, verified bool) (*model.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if subject == "" || len(subject) > 128 || strings.TrimSpace(subject) != subject || !model.IsValidEmail(email) || len(email) > model.UserEmailMaxLength {
		return nil, errors.New("OAuth provider returned an invalid subject or email")
	}
	if username == "" {
		username = strings.SplitN(email, "@", 2)[0]
	}
	username = strings.ToLower(strings.TrimSpace(username))
	if !model.IsValidUsername(username) {
		// Reserved/unsupported names must not generate a different username on every login.
		hash := sha256.Sum256([]byte(service + ":" + subject))
		username = service + "_" + hex.EncodeToString(hash[:8])
	}
	u := &model.User{AuthService: service, AuthData: &subject, Email: email, EmailVerified: verified,
		Username: username, FirstName: first, LastName: last}
	check := u.DeepCopy()
	check.PreSave(nil)
	if check.IsValid() != nil {
		return nil, errors.New("OAuth provider returned an invalid user profile")
	}
	return u, nil
}

func SameUser(service string, stored, incoming *model.User) bool {
	return stored != nil && incoming != nil && stored.AuthService == service && incoming.AuthService == service &&
		stored.AuthData != nil && incoming.AuthData != nil && *stored.AuthData != "" && *stored.AuthData == *incoming.AuthData
}
