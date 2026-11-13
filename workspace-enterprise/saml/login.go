package saml

import (
	"errors"
	"net/http"
	"time"

	"github.com/mattermost/enterprise/saml/internal/encryptedresponse"
	saml2 "github.com/mattermost/gosaml2"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

func (s *Service) DoLogin(rctx request.CTX, encoded string, props map[string]string) (result *model.User, assertion *saml2.AssertionInfo, userCreated bool, resultErr *model.AppError) {
	defer func() { localizeError(rctx, resultErr) }()
	sp, cfg, appErr := s.provider(true)
	if appErr != nil {
		return nil, nil, false, appErr
	}
	invalid := func() (*model.User, *saml2.AssertionInfo, bool, *model.AppError) {
		return nil, nil, false, samlError("ent.saml.invalid_response.app_error", http.StatusUnauthorized)
	}
	if len(encoded) == 0 || len(encoded) > 2*1024*1024 {
		return invalid()
	}
	info, err := validatedAssertion(sp, encoded)
	if err != nil || info == nil || len(info.Assertions) != 1 || info.WarningInfo == nil || info.WarningInfo.InvalidTime || info.WarningInfo.NotInAudience {
		return invalid()
	}
	a := info.Assertions[0]
	if (!a.SignatureValidated && !info.ResponseSignatureValidated) || a.ID == "" || len(a.ID) > 512 || a.Version != "2.0" ||
		a.Issuer == nil || a.Issuer.Value != *cfg.IdpDescriptorURL || a.Conditions == nil || len(a.Conditions.AudienceRestrictions) == 0 ||
		a.Subject == nil || a.Subject.SubjectConfirmation == nil || a.Subject.SubjectConfirmation.SubjectConfirmationData == nil {
		return invalid()
	}
	now := time.Now()
	expires, err := time.Parse(time.RFC3339, a.Conditions.NotOnOrAfter)
	if err != nil || !now.Before(expires) || expires.After(now.Add(24*time.Hour)) || a.IssueInstant.IsZero() || a.IssueInstant.After(now.Add(time.Minute)) {
		return invalid()
	}
	confirmation := a.Subject.SubjectConfirmation.SubjectConfirmationData
	confirmationExpires, err := time.Parse(time.RFC3339, confirmation.NotOnOrAfter)
	if err != nil || !now.Before(confirmationExpires) {
		return invalid()
	}
	if info.SessionNotOnOrAfter != nil && !now.Before(*info.SessionNotOnOrAfter) {
		return invalid()
	}
	// Reject duplicate attribute names instead of silently accepting the last value.
	seen := map[string]bool{}
	if a.AttributeStatement == nil {
		return invalid()
	}
	for _, attr := range a.AttributeStatement.Attributes {
		if attr.Name == "" || seen[attr.Name] {
			return invalid()
		}
		seen[attr.Name] = true
	}
	mapped, appErr := mapUser(info, cfg)
	if appErr != nil {
		return nil, nil, false, appErr
	}
	if *cfg.EnableSyncWithLdap && *cfg.EnableSyncWithLdapIncludeAuth {
		resolver, ok := s.app.Ldap().(interface {
			ResolveSAMLUser(request.CTX, string, string, bool) (*model.User, *model.AppError)
		})
		if !ok {
			return nil, nil, false, samlError("ent.saml.ldap_sync_unavailable.app_error", http.StatusNotImplemented)
		}
		useID := *cfg.IdAttribute != ""
		ldapUser, lookupErr := resolver.ResolveSAMLUser(rctx, *mapped.AuthData, mapped.Email, useID)
		if lookupErr != nil {
			return nil, nil, false, lookupErr
		}
		if useID {
			mapped.AuthData = new(*ldapUser.AuthData)
		} else {
			mapped.AuthData = new(ldapUser.Email)
		}
		mapped.Email, mapped.Username = ldapUser.Email, ldapUser.Username
		mapped.FirstName, mapped.LastName = ldapUser.FirstName, ldapUser.LastName
		mapped.Nickname, mapped.Position = ldapUser.Nickname, ldapUser.Position
	}
	if !licensed(s.app.License()) || !*s.app.Config().SamlSettings.Enable || digest(cfg) != digest(s.app.Config().SamlSettings) {
		return invalid()
	}
	if err = s.consumeAssertion(rctx, a.ID, a.Issuer.Value, confirmation.InResponseTo, expires, cfg, props); err != nil {
		return invalid()
	}
	u, created, appErr := s.persistUser(rctx, cfg, mapped, info, props)
	if appErr != nil {
		return nil, nil, false, appErr
	}
	// The existing web handler applies account/MFA criteria, audit logging,
	// invitation handling and session creation after this method returns.
	return u, info, created, nil
}

// gosaml2 decrypts untrusted XML and some malformed cipher blocks can panic.
// Contain that library boundary and return the same generic authentication
// failure as an invalid signature. Never expose XML or decryption details.
func validatedAssertion(sp *saml2.SAMLServiceProvider, encoded string) (info *saml2.AssertionInfo, err error) {
	defer func() {
		if recover() != nil {
			info, err = nil, errors.New("invalid SAML response")
		}
	}()
	if sp.SPKeyStore != nil {
		return encryptedresponse.RetrieveAssertionInfo(sp, encoded)
	}
	return sp.RetrieveAssertionInfo(encoded)
}
