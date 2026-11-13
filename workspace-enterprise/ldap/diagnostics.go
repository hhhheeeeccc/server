package ldap

import (
	"bytes"
	"encoding/json"
	"net/http"

	ldapclient "github.com/mattermost/ldap"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app/platform"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
)

type Diagnostics struct {
	platform *platform.PlatformService
}

var _ einterfaces.LdapDiagnosticInterface = (*Diagnostics)(nil)

func NewDiagnostics(ps *platform.PlatformService) einterfaces.LdapDiagnosticInterface {
	return &Diagnostics{platform: ps}
}

func (s *Diagnostics) licensed() *model.AppError {
	license := s.platform.License()
	if license == nil || license.Features == nil || !model.SafeDereference(license.Features.LDAP) {
		return model.NewAppError("LDAP.Diagnostics", "ent.ldap.disabled.app_error", nil, "", http.StatusForbidden)
	}
	return nil
}

// The wizard submits partial, unsaved settings. Only omitted fields inherit
// saved values; empty filters and mappings must remain explicitly empty.
func mergeTestSettings(saved, submitted model.LdapSettings) (model.LdapSettings, *model.AppError) {
	// JSON decoding reuses non-nil pointer fields. Deep-copy first so testing
	// unsaved settings cannot mutate the live configuration through those pointers.
	var result model.LdapSettings
	raw, err := json.Marshal(saved)
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	if err != nil {
		return result, model.NewAppError("LDAP.Diagnostics", model.PayloadParseError, nil, "", http.StatusBadRequest).Wrap(err)
	}
	raw, err = json.Marshal(submitted)
	if err != nil {
		return result, model.NewAppError("LDAP.Diagnostics", model.PayloadParseError, nil, "", http.StatusBadRequest).Wrap(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return result, model.NewAppError("LDAP.Diagnostics", model.PayloadParseError, nil, "", http.StatusBadRequest).Wrap(err)
	}
	for name, value := range fields {
		if bytes.Equal(value, []byte("null")) {
			delete(fields, name)
		}
	}
	raw, err = json.Marshal(fields)
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	if err != nil {
		return result, model.NewAppError("LDAP.Diagnostics", model.PayloadParseError, nil, "", http.StatusBadRequest).Wrap(err)
	}
	result.SetDefaults()
	password := model.SafeDereference(submitted.BindPassword)
	if password == "" || password == model.FakeSetting {
		// Never send a saved secret to a newly submitted destination. The
		// public App API also checks this; keep direct interface calls safe.
		if model.SafeDereference(saved.BindPassword) != "" &&
			(*result.LdapServer != model.SafeDereference(saved.LdapServer) ||
				*result.LdapPort != model.SafeDereference(saved.LdapPort) ||
				*result.ConnectionSecurity != model.SafeDereference(saved.ConnectionSecurity) ||
				*result.BindUsername != model.SafeDereference(saved.BindUsername)) {
			return result, model.NewAppError("LDAP.Diagnostics", "api.ldap.test.reenter_password", nil, "", http.StatusBadRequest)
		}
		result.BindPassword = model.NewPointer(model.SafeDereference(saved.BindPassword))
	}
	return result, nil
}

func diagnosticConnectionError(err error) *model.AppError {
	return model.NewAppError("LDAP.Diagnostics", "ent.ldap.do_login.unable_to_connect.app_error", nil, "", http.StatusBadGateway).Wrap(err)
}

func (s *Diagnostics) RunTestConnection(rctx request.CTX, submitted model.LdapSettings) *model.AppError {
	if appErr := s.licensed(); appErr != nil {
		return appErr
	}
	settings, appErr := mergeTestSettings(s.platform.Config().LdapSettings, submitted)
	if appErr != nil {
		return appErr
	}
	d, err := connect(rctx, settings, s.platform.GetConfigFile)
	if err != nil {
		return diagnosticConnectionError(err)
	}
	d.close()
	return nil
}

func (s *Diagnostics) RunTest(rctx request.CTX) *model.AppError {
	if appErr := s.licensed(); appErr != nil {
		return appErr
	}
	settings := s.platform.Config().LdapSettings
	if !model.SafeDereference(settings.Enable) && !model.SafeDereference(settings.EnableSync) {
		return model.NewAppError("LDAP.Diagnostics", "ent.ldap.disabled.app_error", nil, "", http.StatusForbidden)
	}
	d, err := connect(rctx, settings, s.platform.GetConfigFile)
	if err != nil {
		return diagnosticConnectionError(err)
	}
	defer d.close()
	_, err = d.search(settings, *settings.BaseDN, "(objectClass=*)", []string{"objectClass"}, ldapclient.ScopeBaseObject)
	if err != nil {
		return diagnosticConnectionError(err)
	}
	return nil
}

func (s *Diagnostics) GetVendorNameAndVendorVersion(rctx request.CTX) (string, string, error) {
	if appErr := s.licensed(); appErr != nil {
		return "", "", appErr
	}
	settings := s.platform.Config().LdapSettings
	d, err := connect(rctx, settings, s.platform.GetConfigFile)
	if err != nil {
		return "", "", err
	}
	defer d.close()
	entries, err := d.search(settings, "", "(objectClass=*)", []string{"vendorName", "vendorVersion"}, ldapclient.ScopeBaseObject)
	if err != nil || len(entries) == 0 {
		return "", "", err
	}
	return attributeValue(entries[0], "vendorName"), attributeValue(entries[0], "vendorVersion"), nil
}

type attributeMapping struct{ name, attribute string }

func userMappings(settings model.LdapSettings) []attributeMapping {
	return []attributeMapping{
		{"IdAttribute", *settings.IdAttribute}, {"UsernameAttribute", *settings.UsernameAttribute},
		{"LoginIdAttribute", *settings.LoginIdAttribute}, {"EmailAttribute", *settings.EmailAttribute},
		{"FirstNameAttribute", *settings.FirstNameAttribute}, {"LastNameAttribute", *settings.LastNameAttribute},
		{"NicknameAttribute", *settings.NicknameAttribute}, {"PositionAttribute", *settings.PositionAttribute},
		{"PictureAttribute", *settings.PictureAttribute},
	}
}

func sampleEntry(entry *ldapclient.Entry, settings model.LdapSettings, group bool) model.LdapSampleEntry {
	if group {
		return model.LdapSampleEntry{DN: entry.DN, ID: entryID(entry, *settings.GroupIdAttribute), DisplayName: attributeValue(entry, *settings.GroupDisplayNameAttribute)}
	}
	return model.LdapSampleEntry{DN: entry.DN, ID: entryID(entry, *settings.IdAttribute),
		Username: attributeValue(entry, *settings.UsernameAttribute), Email: attributeValue(entry, *settings.EmailAttribute),
		FirstName: attributeValue(entry, *settings.FirstNameAttribute), LastName: attributeValue(entry, *settings.LastNameAttribute)}
}

func (s *Diagnostics) RunTestDiagnostics(rctx request.CTX, testType model.LdapDiagnosticTestType, submitted model.LdapSettings) ([]model.LdapDiagnosticResult, *model.AppError) {
	if appErr := s.licensed(); appErr != nil {
		return nil, appErr
	}
	if !testType.IsValid() {
		return nil, model.NewAppError("LDAP.Diagnostics", "api.context.invalid_url_param.app_error", map[string]any{"Name": "test"}, "", http.StatusBadRequest)
	}
	settings, appErr := mergeTestSettings(s.platform.Config().LdapSettings, submitted)
	if appErr != nil {
		return nil, appErr
	}
	d, err := connect(rctx, settings, s.platform.GetConfigFile)
	if err != nil {
		return nil, diagnosticConnectionError(err)
	}
	defer d.close()
	results := []model.LdapDiagnosticResult{}
	group := testType == model.LdapDiagnosticTestTypeGroupAttributes
	mappings := userMappings(settings)
	if group {
		mappings = []attributeMapping{{"GroupIdAttribute", *settings.GroupIdAttribute}, {"GroupDisplayNameAttribute", *settings.GroupDisplayNameAttribute}}
	}
	attributes := []string{"objectClass"}
	for _, mapping := range mappings {
		if mapping.attribute != "" && attributeName.MatchString(mapping.attribute) {
			attributes = append(attributes, mapping.attribute)
		}
	}
	if testType == model.LdapDiagnosticTestTypeFilters {
		attributes = append(attributes, "objectClass")
		for _, name := range []string{*settings.GroupIdAttribute, *settings.GroupDisplayNameAttribute} {
			if attributeName.MatchString(name) {
				attributes = append(attributes, name)
			}
		}
		filters := []attributeMapping{{"BaseDN", "(objectClass=*)"}, {"UserFilter", *settings.UserFilter}, {"GroupFilter", groupFilter(settings)}, {"GuestFilter", *settings.GuestFilter}}
		if *settings.EnableAdminFilter {
			filters = append(filters, attributeMapping{"AdminFilter", *settings.AdminFilter})
		}
		for _, filter := range filters {
			result := model.LdapDiagnosticResult{TestName: filter.name, TestValue: filter.attribute, SampleResults: []model.LdapSampleEntry{}}
			if filter.name == "BaseDN" {
				result.TestValue = *settings.BaseDN
			}
			if filter.name == "UserFilter" && filter.attribute == "" {
				result.TestValue = "(objectClass=*)"
			}
			if filter.attribute == "" && filter.name != "UserFilter" {
				result.Message = "Filter is not configured."
				results = append(results, result)
				continue
			}
			entries, err := d.search(settings, *settings.BaseDN, filter.attribute, attributes, ldapclient.ScopeWholeSubtree)
			if err != nil {
				result.Error = err.Error()
			} else {
				result.TotalCount = len(entries)
				for _, entry := range entries[:min(5, len(entries))] {
					result.SampleResults = append(result.SampleResults, sampleEntry(entry, settings, filter.name == "GroupFilter"))
				}
			}
			results = append(results, result)
		}
		return results, nil
	}
	filter := *settings.UserFilter
	if group {
		filter = groupFilter(settings)
	}
	entries, searchErr := d.search(settings, *settings.BaseDN, filter, attributes, ldapclient.ScopeWholeSubtree)
	for _, mapping := range mappings {
		result := model.LdapDiagnosticResult{TestName: mapping.name, TestValue: mapping.attribute, TotalCount: len(entries), SampleResults: []model.LdapSampleEntry{}}
		switch {
		case searchErr != nil:
			result.Error = searchErr.Error()
		case mapping.attribute == "":
			result.Message = "Attribute is not configured."
		case !attributeName.MatchString(mapping.attribute):
			result.Error = "Invalid LDAP attribute name."
		default:
			for _, entry := range entries {
				if attributeValue(entry, mapping.attribute) == "" {
					continue
				}
				result.EntriesWithValue++
				if len(result.SampleResults) < 5 {
					result.SampleResults = append(result.SampleResults, sampleEntry(entry, settings, group))
				}
			}
		}
		results = append(results, result)
	}
	return results, nil
}
