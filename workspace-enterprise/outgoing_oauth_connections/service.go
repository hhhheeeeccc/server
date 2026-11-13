package outgoing_oauth_connections

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/store"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
)

const maxTokenResponseSize = 1024 * 1024

type Service struct {
	app *app.App
}

var _ einterfaces.OutgoingOAuthConnectionInterface = (*Service)(nil)

func New(a *app.App) einterfaces.OutgoingOAuthConnectionInterface {
	return &Service{app: a}
}

func (s *Service) DeleteConnection(rctx request.CTX, id string) *model.AppError {
	if appErr := s.requireLicense("DeleteConnection"); appErr != nil {
		return appErr
	}
	if err := s.app.Srv().Store().OutgoingOAuthConnection().DeleteConnection(rctx, id); err != nil {
		return s.storeError("DeleteConnection", err)
	}
	return nil
}

func (s *Service) GetConnection(rctx request.CTX, id string) (*model.OutgoingOAuthConnection, *model.AppError) {
	if appErr := s.requireLicense("GetConnection"); appErr != nil {
		return nil, appErr
	}
	connection, err := s.app.Srv().Store().OutgoingOAuthConnection().GetConnection(rctx, id)
	if err != nil {
		return nil, s.storeError("GetConnection", err)
	}
	return connection, nil
}

func (s *Service) GetConnections(rctx request.CTX, filters model.OutgoingOAuthConnectionGetConnectionsFilter) ([]*model.OutgoingOAuthConnection, *model.AppError) {
	if appErr := s.requireLicense("GetConnections"); appErr != nil {
		return nil, appErr
	}
	connections, err := s.app.Srv().Store().OutgoingOAuthConnection().GetConnections(rctx, filters)
	if err != nil {
		return nil, s.storeError("GetConnections", err)
	}
	return connections, nil
}

func (s *Service) SaveConnection(rctx request.CTX, connection *model.OutgoingOAuthConnection) (*model.OutgoingOAuthConnection, *model.AppError) {
	if appErr := s.requireLicense("SaveConnection"); appErr != nil {
		return nil, appErr
	}
	if connection == nil {
		return nil, model.NewAppError("OutgoingOAuthConnection.SaveConnection", "model.outgoing_oauth_connection.is_valid.id.error", nil, "connection is nil", http.StatusBadRequest)
	}

	saved, err := s.app.Srv().Store().OutgoingOAuthConnection().SaveConnection(rctx, connection)
	if err != nil {
		return nil, s.storeError("SaveConnection", err)
	}
	if saved.GrantType == model.OutgoingOAuthConnectionGrantTypePassword {
		saved, err = s.app.Srv().Store().OutgoingOAuthConnection().UpdateConnection(rctx, saved)
		if err != nil {
			_ = s.app.Srv().Store().OutgoingOAuthConnection().DeleteConnection(rctx, saved.Id)
			return nil, s.storeError("SaveConnection", err)
		}
	}
	return saved, nil
}

func (s *Service) UpdateConnection(rctx request.CTX, connection *model.OutgoingOAuthConnection) (*model.OutgoingOAuthConnection, *model.AppError) {
	if appErr := s.requireLicense("UpdateConnection"); appErr != nil {
		return nil, appErr
	}
	if connection == nil {
		return nil, model.NewAppError("OutgoingOAuthConnection.UpdateConnection", "model.outgoing_oauth_connection.is_valid.id.error", nil, "connection is nil", http.StatusBadRequest)
	}
	updated, err := s.app.Srv().Store().OutgoingOAuthConnection().UpdateConnection(rctx, connection)
	if err != nil {
		return nil, s.storeError("UpdateConnection", err)
	}
	return updated, nil
}

func (s *Service) SanitizeConnection(connection *model.OutgoingOAuthConnection) {
	if connection != nil {
		connection.Sanitize()
	}
}

func (s *Service) SanitizeConnections(connections []*model.OutgoingOAuthConnection) {
	for _, connection := range connections {
		s.SanitizeConnection(connection)
	}
}

func (s *Service) GetConnectionForAudience(rctx request.CTX, targetURL string) (*model.OutgoingOAuthConnection, *model.AppError) {
	if appErr := s.requireLicense("GetConnectionForAudience"); appErr != nil {
		return nil, appErr
	}

	var best *model.OutgoingOAuthConnection
	bestMatchLength := -1
	fromID := ""
	for {
		connections, err := s.app.Srv().Store().OutgoingOAuthConnection().GetConnections(rctx, model.OutgoingOAuthConnectionGetConnectionsFilter{
			OffsetId: fromID,
			Limit:    100,
		})
		if err != nil {
			return nil, s.storeError("GetConnectionForAudience", err)
		}
		for _, connection := range connections {
			for _, audience := range connection.Audiences {
				if matchLength, matches := audienceMatchLength(audience, targetURL); matches && matchLength > bestMatchLength {
					best = connection
					bestMatchLength = matchLength
				}
			}
		}
		if len(connections) < 100 {
			break
		}
		fromID = connections[len(connections)-1].Id
	}
	return best, nil
}

func (s *Service) RetrieveTokenForConnection(rctx request.CTX, connection *model.OutgoingOAuthConnection) (*model.OutgoingOAuthConnectionToken, *model.AppError) {
	if appErr := s.requireLicense("RetrieveTokenForConnection"); appErr != nil {
		return nil, appErr
	}
	if appErr := validateTokenConnection(connection); appErr != nil {
		return nil, appErr
	}

	token, err := retrieveToken(rctx.Context(), s.app.HTTPService().MakeClient(false), connection)
	if err != nil {
		status := http.StatusBadGateway
		translationID := "api.context.outgoing_oauth_connection.validate_connection_credentials.app_error"
		var endpointErr *tokenEndpointStatusError
		if errors.As(err, &endpointErr) && endpointErr.status >= http.StatusBadRequest && endpointErr.status < http.StatusInternalServerError {
			status = http.StatusBadRequest
			translationID = "api.context.outgoing_oauth_connection.validate_connection_credentials.input_error"
		}
		// HTTP errors can include the full token URL, including credentials in
		// its query string. Keep raw transport errors out of AppError responses
		// and logs; only the safe status classification is returned.
		return nil, model.NewAppError("OutgoingOAuthConnection.RetrieveTokenForConnection", translationID, nil, "token request failed", status)
	}
	return token, nil
}

func (s *Service) requireLicense(method string) *model.AppError {
	license := s.app.License()
	if license != nil && license.Features != nil && model.SafeDereference(license.Features.OutgoingOAuthConnections) {
		return nil
	}
	return model.NewAppError("OutgoingOAuthConnection."+method, "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
}

func (s *Service) storeError(method string, err error) *model.AppError {
	status := http.StatusInternalServerError
	var notFound *store.ErrNotFound
	if errors.As(err, &notFound) {
		status = http.StatusNotFound
	}
	var appErr *model.AppError
	if errors.As(err, &appErr) {
		return appErr
	}
	translationID := "api.context.outgoing_oauth_connection.list_connections.app_error"
	switch method {
	case "SaveConnection":
		translationID = "api.context.outgoing_oauth_connection.create_connection.app_error"
	case "UpdateConnection":
		translationID = "api.context.outgoing_oauth_connection.update_connection.app_error"
	case "DeleteConnection":
		translationID = "api.context.outgoing_oauth_connection.delete_connection.app_error"
	}
	return model.NewAppError("OutgoingOAuthConnection."+method, translationID, nil, "", status).Wrap(err)
}

// validateTokenConnection validates only the fields needed to request a token.
// The credentials validation endpoint also accepts a connection that has not
// been saved yet, so database fields such as Id and CreateAt are deliberately
// not required here.
func validateTokenConnection(connection *model.OutgoingOAuthConnection) *model.AppError {
	if connection == nil {
		return tokenError("connection is nil", nil)
	}
	if connection.ClientId == "" || utf8.RuneCountInString(connection.ClientId) > 255 {
		return model.NewAppError("OutgoingOAuthConnection.RetrieveTokenForConnection", "model.outgoing_oauth_connection.is_valid.client_id.error", nil, "", http.StatusBadRequest)
	}
	if connection.ClientSecret == "" || utf8.RuneCountInString(connection.ClientSecret) > 255 {
		return model.NewAppError("OutgoingOAuthConnection.RetrieveTokenForConnection", "model.outgoing_oauth_connection.is_valid.client_secret.error", nil, "", http.StatusBadRequest)
	}
	if !model.IsValidHTTPURL(connection.OAuthTokenURL) || utf8.RuneCountInString(connection.OAuthTokenURL) > 256 {
		return model.NewAppError("OutgoingOAuthConnection.RetrieveTokenForConnection", "model.outgoing_oauth_connection.is_valid.oauth_token_url.error", nil, "", http.StatusBadRequest)
	}
	if appErr := connection.HasValidGrantType(); appErr != nil {
		return appErr
	}
	return nil
}

func audienceMatchLength(audience, targetURL string) (int, bool) {
	if strings.HasSuffix(audience, "*") {
		prefix := strings.TrimSuffix(audience, "*")
		if !strings.HasPrefix(targetURL, prefix) {
			return len(prefix), false
		}
		// A textual prefix can cross the authority boundary: an audience of
		// https://example.com* would also match https://example.com.evil/.
		// Only the path and query may be expanded by the wildcard.
		base, baseErr := url.Parse(prefix)
		target, targetErr := url.Parse(targetURL)
		if baseErr != nil || targetErr != nil || base.Host == "" || target.Host == "" {
			return len(prefix), false
		}
		if strings.Contains(target.Path, `\`) {
			return len(prefix), false
		}
		// A target such as /hooks/../admin has the textual prefix but can be
		// normalized outside it by the receiver. url.Parse decodes %2e segments.
		for _, segment := range strings.Split(target.Path, "/") {
			if segment == "." || segment == ".." {
				return len(prefix), false
			}
		}
		return len(prefix), strings.EqualFold(base.Scheme, target.Scheme) &&
			strings.EqualFold(base.Hostname(), target.Hostname()) && base.Port() == target.Port()
	}
	return len(audience), audience == targetURL
}

func retrieveToken(ctx context.Context, client *http.Client, connection *model.OutgoingOAuthConnection) (*model.OutgoingOAuthConnectionToken, error) {
	form := url.Values{
		"grant_type":    []string{string(connection.GrantType)},
		"client_id":     []string{connection.ClientId},
		"client_secret": []string{connection.ClientSecret},
	}
	if connection.GrantType == model.OutgoingOAuthConnectionGrantTypePassword {
		form.Set("username", model.SafeDereference(connection.CredentialsUsername))
		form.Set("password", model.SafeDereference(connection.CredentialsPassword))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, connection.OAuthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	// A 307/308 response replays the POST body, including the client secret.
	// Keep redirects on the configured token endpoint's origin so an OAuth
	// server cannot forward those credentials to another host or port.
	redirectClient := *client
	previousCheckRedirect := client.CheckRedirect
	redirectClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if !strings.EqualFold(next.URL.Scheme, req.URL.Scheme) ||
			!strings.EqualFold(next.URL.Hostname(), req.URL.Hostname()) ||
			next.URL.Port() != req.URL.Port() {
			return errors.New("token endpoint redirected to another origin")
		}
		if previousCheckRedirect != nil {
			return previousCheckRedirect(next, via)
		}
		if len(via) >= 10 {
			return errors.New("token endpoint redirected too many times")
		}
		return nil
	}
	response, err := redirectClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, &tokenEndpointStatusError{status: response.StatusCode}
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxTokenResponseSize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxTokenResponseSize {
		return nil, fmt.Errorf("token response exceeds %d bytes", maxTokenResponseSize)
	}
	var responseBody struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err = json.Unmarshal(body, &responseBody); err != nil {
		return nil, err
	}
	if responseBody.AccessToken == "" || responseBody.TokenType == "" {
		return nil, fmt.Errorf("token endpoint response is missing access_token or token_type")
	}
	return &model.OutgoingOAuthConnectionToken{
		AccessToken: responseBody.AccessToken,
		TokenType:   responseBody.TokenType,
	}, nil
}

type tokenEndpointStatusError struct {
	status int
}

func (e *tokenEndpointStatusError) Error() string {
	return fmt.Sprintf("token endpoint returned status %d", e.status)
}

func tokenError(detail string, err error) *model.AppError {
	appErr := model.NewAppError("OutgoingOAuthConnection.RetrieveTokenForConnection", "api.context.outgoing_oauth_connection.validate_connection_credentials.input_error", nil, detail, http.StatusBadRequest)
	if err != nil {
		appErr.Wrap(err)
	}
	return appErr
}
