package access_control

import (
	"cmp"
	"slices"
	"strings"

	"github.com/google/cel-go/common/operators"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

type simulationSession struct {
	result     model.PolicySimulationSession
	attributes map[string]any
}

func (s *Service) simulationSessions(rctx request.CTX, input model.PolicySimulationUserOverride) ([]simulationSession, *model.AppError) {
	var sessions []*model.Session
	// Other users' session identities and device attributes are visible only
	// to callers with the same system permission required by the REST API.
	if caller := rctx.Session(); caller != nil && s.app.SessionHasPermissionTo(*caller, model.PermissionManageSystem) {
		var appErr *model.AppError
		sessions, appErr = s.app.GetSessions(rctx, input.UserID)
		if appErr != nil {
			return nil, appErr
		}
	}
	slices.SortFunc(sessions, func(a, b *model.Session) int {
		if order := cmp.Compare(b.LastActivityAt, a.LastActivityAt); order != 0 {
			return order
		}
		return strings.Compare(a.Id, b.Id)
	})
	result := []simulationSession{}
	for _, session := range sessions {
		if session.UserId != input.UserID || session.IsExpired() || session.IsOAuth || session.Local {
			continue
		}
		switch session.Props[model.SessionPropType] {
		case model.SessionTypeUserAccessToken, model.SessionTypeCloudKey, model.SessionTypeRemoteclusterToken:
			continue
		}
		attributes, appErr := s.app.GetSessionAttributes(rctx, session.Id)
		if appErr != nil {
			return nil, appErr
		}
		entry := newSimulationSession(attributes, input.SessionOverrides)
		entry.result.ID = session.Id
		entry.result.LastActiveAt = session.LastActivityAt
		deviceOS, _ := attributes[model.SessionAttributesPropertyFieldUserAgentOS].(string)
		browser, _ := attributes[model.SessionAttributesPropertyFieldUserAgentBrowserName].(string)
		entry.result.Device = strings.TrimSpace(deviceOS + " " + browser)
		result = append(result, entry)
	}
	if len(result) == 0 {
		// No real target session is available, or the caller may not inspect
		// one. An explicit hypothetical override is still useful; never use
		// the requesting administrator's device as the target's baseline.
		result = append(result, newSimulationSession(nil, input.SessionOverrides))
	}
	return result, nil
}

func newSimulationSession(baseline, overrides map[string]any) simulationSession {
	attributes := make(map[string]any, len(baseline)+len(overrides))
	for key, value := range baseline {
		attributes[key] = value
	}
	for key, value := range overrides {
		attributes[key] = value
	}
	return simulationSession{attributes: attributes, result: model.PolicySimulationSession{Attributes: stringAttributes(attributes)}}
}

func (s *Service) simulationDecisions(rctx request.CTX, params model.PolicySimulationByUsersParams, draft *model.AccessControlPolicy, subject model.Subject, resourceAttrs map[string]any) (map[string]model.PolicySimulationActionDecision, *model.AppError) {
	decisions := make(map[string]model.PolicySimulationActionDecision, len(params.Actions))
	resource := model.Resource{ID: params.ChannelID, Type: draft.Type}
	if params.ChannelID != "" {
		resource.Type = model.AccessControlPolicyTypeChannel
	}
	for _, action := range params.Actions {
		req := model.AccessRequest{Subject: subject, Resource: resource, Action: action}
		decision, err := s.simulateAction(rctx, params, draft, req, resourceAttrs)
		if err != nil {
			return nil, internalError("AccessControl.SimulatePolicyForUsers", "enterprise.access_control.evaluate.app_error", err)
		}
		if len(subject.Session) == 0 {
			usesSession, err := s.simulationUsesSession(rctx, params, draft, req)
			if err != nil {
				return nil, internalError("AccessControl.SimulatePolicyForUsers", "enterprise.access_control.evaluate.app_error", err)
			}
			if usesSession {
				// Keep the raw decision fail-closed as well as rendering a
				// neutral marker. The core's this_rule response filter drops
				// this marker; a true decision could then look like a grant
				// for a system administrator with no device snapshot.
				decision = model.PolicySimulationActionDecision{Decision: false, Blame: []model.PolicySimulationBlame{{Source: model.PolicySimulationBlameSourceNoSessionData}}}
			}
		}
		for i := range decision.Blame {
			if decision.Blame[i].Expression == "" {
				continue
			}
			normalized, appErr := s.normalizeRankExpression(rctx, decision.Blame[i].Expression)
			if appErr != nil {
				return nil, appErr
			}
			decision.Blame[i].Expression = normalized
		}
		decisions[action] = decision
	}
	return decisions, nil
}

// Inspect the applicable policy graph, including imports and the saved lane
// when requested. A user/profile-only action remains evaluable without a
// device; a session-dependent action must not invent a device snapshot.
func (s *Service) simulationUsesSession(rctx request.CTX, params model.PolicySimulationByUsersParams, draft *model.AccessControlPolicy, req model.AccessRequest) (bool, error) {
	policies := []*model.AccessControlPolicy{draft}
	if params.EvaluationScope == model.PolicyEvaluationScopeAll {
		saved, appErr := s.policiesForRequest(rctx, req)
		if appErr != nil {
			return false, appErr
		}
		policies = append(policies, saved...)
	}
	visited := map[string]bool{}
	var inspect func(*model.AccessControlPolicy) (bool, error)
	inspect = func(policy *model.AccessControlPolicy) (bool, error) {
		if policy != nil && draft.ID != "" && policy.ID == draft.ID {
			policy = draft
		}
		if policy == nil || visited[policy.ID] || (len(policy.Roles) > 0 && !subjectHasAnyRole(req.Subject, policy.Roles)) {
			return false, nil
		}
		visited[policy.ID] = true
		for _, rule := range policy.Rules {
			if rule.IsMetadataCarrier() || !slices.Contains(rule.Actions, req.Action) || !ruleAppliesToRole(req.Subject, rule.Role) {
				continue
			}
			checked, err := s.engine.checkedExpression(rule.Expression)
			if err != nil {
				return false, err
			}
			if expressionUsesSession(checked.Expr) {
				return true, nil
			}
		}
		if params.EvaluationScope == model.PolicyEvaluationScopeAll {
			for _, id := range policy.Imports {
				parent := draft
				if id != draft.ID {
					var err error
					parent, err = s.app.Srv().Store().AccessControlPolicy().Get(rctx, id)
					if err != nil {
						return false, err
					}
				}
				if found, err := inspect(parent); found || err != nil {
					return found, err
				}
			}
		}
		return false, nil
	}
	for _, policy := range policies {
		if found, err := inspect(policy); found || err != nil {
			return found, err
		}
	}
	return false, nil
}

func expressionUsesSession(expression *exprpb.Expr) bool {
	found := false
	walkExpr(expression, func(node *exprpb.Expr) {
		if path, ok := selectPath(node); ok && (path == "user.session" || strings.HasPrefix(path, "user.session.")) {
			found = true
		}
		// Also cover CEL's bracket form, including a dynamic key on user.
		if call := node.GetCallExpr(); call != nil && call.Function == operators.Index && len(call.Args) == 2 {
			if path, ok := selectPath(call.Args[0]); ok && path == "user" {
				key := call.Args[1].GetConstExpr()
				if key == nil || key.GetStringValue() == "session" {
					found = true
				}
			}
		}
	})
	return found
}
