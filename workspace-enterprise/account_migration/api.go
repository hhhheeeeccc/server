package account_migration

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

const explicitSAMLRoute = model.APIURLSuffix + "/workspace/migrate_auth/saml/identities"

type explicitSAMLRequest struct {
	From       string         `json:"from"`
	DryRun     bool           `json:"dry_run"`
	Identities []SAMLIdentity `json:"identities"`
}

// RegisterAPI adds the administrative route needed when the SAML IdAttribute
// is not derivable from the native migration API's email-to-username map.
func RegisterAPI(a *app.App) {
	handler := func(c *web.Context, w http.ResponseWriter, r *http.Request) {
		if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionManageSystem) {
			c.SetPermissionError(model.PermissionManageSystem)
			return
		}
		var input explicitSAMLRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 10<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			c.SetInvalidParamWithErr("body", err)
			return
		}
		var trailing any
		if decoder.Decode(&trailing) != io.EOF || len(input.Identities) == 0 {
			c.SetInvalidParam("body")
			return
		}
		auditRec := c.MakeAuditRecord(model.AuditEventMigrateAuthToSaml, model.AuditStatusFail)
		defer c.LogAuditRec(auditRec)
		model.AddEventParameterToAuditRec(auditRec, "from", input.From)
		model.AddEventParameterToAuditRec(auditRec, "dry_run", input.DryRun)
		model.AddEventParameterToAuditRec(auditRec, "identity_count", len(input.Identities))
		service, ok := c.App.AccountMigration().(*Service)
		if !ok {
			c.Err = migrationError("api.admin.saml.not_available.app_error", http.StatusNotImplemented)
			return
		}
		if appErr := service.MigrateToSamlIdentities(c.AppContext, input.From, input.Identities, input.DryRun); appErr != nil {
			c.Err = appErr
			return
		}
		auditRec.Success()
		w.WriteHeader(http.StatusOK)
	}
	a.Srv().Router.Handle(explicitSAMLRoute, &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: true,
		RequireMfa:     true,
	}).Methods(http.MethodPost)
}
