// Package access_control supplies Mattermost's Enterprise ABAC service.
package access_control

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/store"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
)

// Service implements both the policy administration point and policy decision
// point interfaces expected by the Mattermost server.
type Service struct {
	app *app.App

	initOnce sync.Once
	engine   *engine
	initErr  error
}

var _ einterfaces.AccessControlServiceInterface = (*Service)(nil)

func New(a *app.App) einterfaces.AccessControlServiceInterface {
	return &Service{app: a}
}

func (s *Service) Init(_ request.CTX) *model.AppError {
	s.initOnce.Do(func() {
		s.engine, s.initErr = newEngine()
	})
	if s.initErr != nil {
		return internalError("AccessControl.Init", "enterprise.access_control.init.app_error", s.initErr)
	}
	return nil
}

func (s *Service) ensureEngine(rctx request.CTX) *model.AppError {
	return s.Init(rctx)
}

func (s *Service) requireLicense(where string) *model.AppError {
	if s.app == nil || s.app.License() == nil {
		return model.NewAppError(where, "api.license.upgrade_needed.app_error", nil, "an Enterprise license is required", http.StatusForbidden)
	}
	return nil
}

func (s *Service) SavePolicy(rctx request.CTX, policy *model.AccessControlPolicy) (*model.AccessControlPolicy, *model.AppError) {
	if appErr := s.requireLicense("AccessControl.SavePolicy"); appErr != nil {
		return nil, appErr
	}
	if appErr := s.ensureEngine(rctx); appErr != nil {
		return nil, appErr
	}
	if policy == nil {
		return nil, badRequest("AccessControl.SavePolicy", "enterprise.access_control.policy_required.app_error", "policy is required")
	}

	policy = clonePolicy(policy)
	for i, rule := range policy.Rules {
		// Import-only resource policies keep their local auto-add setting in an
		// expression-less membership metadata carrier. It is configuration, not
		// an effective CEL rule, so it must not be parsed or transformed.
		if rule.IsMetadataCarrier() {
			continue
		}
		if issues := s.engine.check(rule.Expression); len(issues) != 0 {
			return nil, badRequest("AccessControl.SavePolicy", "enterprise.access_control.invalid_expression.app_error", issues[0].Message)
		}
		if appErr := s.validateRuleAttributes(rctx, rule); appErr != nil {
			return nil, appErr
		}
		storedExpression, appErr := s.desugarRankExpression(rctx, rule.Expression)
		if appErr != nil {
			return nil, appErr
		}
		if issues := s.engine.check(storedExpression); len(issues) != 0 {
			return nil, badRequest("AccessControl.SavePolicy", "enterprise.access_control.invalid_rank_expression.app_error", issues[0].Message)
		}
		policy.Rules[i].Expression = storedExpression
	}

	// The public app currently stamps v0.3 before handing the policy to the
	// enterprise service. Preserve newer rule shapes by raising the stored
	// version when their model validation requires it.
	if model.IsPluginAccessControlPolicyType(policy.Type) {
		policy.Version = model.AccessControlPolicyVersionV0_5
	} else if slices.ContainsFunc(policy.Rules, func(rule model.AccessControlPolicyRule) bool { return rule.Role != "" }) {
		policy.Version = model.AccessControlPolicyVersionV0_4
	} else if policy.Version == "" {
		policy.Version = model.AccessControlPolicyVersionV0_3
	}
	if appErr := policy.IsValid(); appErr != nil {
		return nil, appErr
	}

	for _, importedID := range policy.Imports {
		imported, err := s.app.Srv().Store().AccessControlPolicy().Get(rctx, importedID)
		if err != nil || imported.Type != model.AccessControlPolicyTypeParent {
			if err == nil {
				err = errors.New("imported policy is not a parent policy")
			}
			return nil, badRequest("AccessControl.SavePolicy", "enterprise.access_control.invalid_import.app_error", err.Error())
		}
	}

	saved, handled, err := s.savePolicyActivation(rctx, policy)
	if err == nil && !handled {
		saved, err = s.app.Srv().Store().AccessControlPolicy().Save(rctx, policy)
	}
	if err != nil {
		if errors.Is(err, errPolicyActivationChanged) {
			return nil, model.NewAppError("AccessControl.SavePolicy", "app.pap.save_policy.app_error", nil, err.Error(), http.StatusConflict).Wrap(err)
		}
		var conflict *store.ErrConflict
		if errors.As(err, &conflict) {
			return nil, model.NewAppError("AccessControl.SavePolicy", "app.pap.save_policy.name_exists.app_error", nil, "", http.StatusConflict).Wrap(err)
		}
		return nil, internalError("AccessControl.SavePolicy", "enterprise.access_control.save_policy.app_error", err)
	}
	return s.NormalizePolicy(rctx, saved)
}

func (s *Service) GetPolicy(rctx request.CTX, id string) (*model.AccessControlPolicy, *model.AppError) {
	if appErr := s.requireLicense("AccessControl.GetPolicy"); appErr != nil {
		return nil, appErr
	}
	policy, err := s.app.Srv().Store().AccessControlPolicy().Get(rctx, id)
	if err != nil {
		return nil, storeError("AccessControl.GetPolicy", "enterprise.access_control.get_policy.app_error", err)
	}
	return s.NormalizePolicy(rctx, policy)
}

func (s *Service) DeletePolicy(rctx request.CTX, id string) *model.AppError {
	if appErr := s.requireLicense("AccessControl.DeletePolicy"); appErr != nil {
		// Channel/team archive and permanent-delete paths explicitly fall
		// back to direct store cleanup on 406. Preserve the license lock for
		// API callers while allowing those public-server cleanup paths to
		// remove an orphaned Enterprise policy after a license is removed.
		appErr.StatusCode = http.StatusNotAcceptable
		return appErr
	}
	// A parent cannot be deleted while a channel still imports it. Team
	// assignments are intentionally not part of this documented channel guard.
	policy, err := s.app.Srv().Store().AccessControlPolicy().Get(rctx, id)
	if err != nil && !store.IsErrNotFound(err) {
		return storeError("AccessControl.DeletePolicy", "enterprise.access_control.get_policy.app_error", err)
	}
	if policy != nil && policy.Type == model.AccessControlPolicyTypeParent {
		children, _, err := s.app.Srv().Store().AccessControlPolicy().SearchPolicies(rctx, model.AccessControlPolicySearch{
			Type: model.AccessControlPolicyTypeChannel, ParentID: id, Limit: 1,
		})
		if err != nil {
			return storeError("AccessControl.DeletePolicy", "enterprise.access_control.search_policies.app_error", err)
		}
		if len(children) > 0 {
			return model.NewAppError("AccessControl.DeletePolicy", "app.pap.delete_policy.app_error", nil, "remove channel assignments before deleting this policy", http.StatusBadRequest)
		}
	}
	if err := s.app.Srv().Store().AccessControlPolicy().Delete(rctx, id); err != nil {
		return storeError("AccessControl.DeletePolicy", "enterprise.access_control.delete_policy.app_error", err)
	}
	return nil
}

func (s *Service) GetPoliciesForFieldIDs(rctx request.CTX, fieldIDs []string) ([]*model.AccessControlPolicy, *model.AppError) {
	if appErr := s.requireLicense("AccessControl.GetPoliciesForFieldIDs"); appErr != nil {
		return nil, appErr
	}
	byID := make(map[string]*model.AccessControlPolicy)
	for _, fieldID := range fieldIDs {
		policies, err := s.app.Srv().Store().AccessControlPolicy().GetPoliciesByFieldID(rctx, fieldID)
		if err != nil {
			return nil, internalError("AccessControl.GetPoliciesForFieldIDs", "enterprise.access_control.get_policies_for_field.app_error", err)
		}
		for _, policy := range policies {
			byID[policy.ID] = policy
		}
	}
	result := make([]*model.AccessControlPolicy, 0, len(byID))
	for _, policy := range byID {
		result = append(result, policy)
	}
	slices.SortFunc(result, func(a, b *model.AccessControlPolicy) int { return strings.Compare(a.ID, b.ID) })
	return result, nil
}

func (s *Service) NormalizePolicy(rctx request.CTX, policy *model.AccessControlPolicy) (*model.AccessControlPolicy, *model.AppError) {
	if policy == nil {
		return nil, nil
	}
	normalized := clonePolicy(policy)
	for i, rule := range normalized.Rules {
		if rule.IsMetadataCarrier() {
			continue
		}
		expression, appErr := s.normalizeRankExpression(rctx, rule.Expression)
		if appErr != nil {
			return nil, appErr
		}
		normalized.Rules[i].Expression = expression
	}
	return normalized, nil
}

func (s *Service) CheckExpression(rctx request.CTX, expression string) ([]model.CELExpressionError, *model.AppError) {
	if appErr := s.ensureEngine(rctx); appErr != nil {
		return nil, appErr
	}
	return s.engine.check(expression), nil
}

func (s *Service) ExpressionToVisualAST(rctx request.CTX, expression string) (*model.VisualExpression, *model.AppError) {
	if appErr := s.ensureEngine(rctx); appErr != nil {
		return nil, appErr
	}
	visual, err := s.engine.visualExpression(expression)
	if err != nil {
		return nil, badRequest("AccessControl.ExpressionToVisualAST", "enterprise.access_control.visual_expression.app_error", err.Error())
	}
	return visual, nil
}

func (s *Service) GetPolicyRuleAttributes(rctx request.CTX, policyID, action string) (map[string][]string, *model.AppError) {
	if appErr := s.requireLicense("AccessControl.GetPolicyRuleAttributes"); appErr != nil {
		return nil, appErr
	}
	if appErr := s.ensureEngine(rctx); appErr != nil {
		return nil, appErr
	}
	policy, err := s.app.Srv().Store().AccessControlPolicy().Get(rctx, policyID)
	if err != nil {
		return nil, storeError("AccessControl.GetPolicyRuleAttributes", "enterprise.access_control.get_policy.app_error", err)
	}

	result := make(map[string][]string)
	visited := make(map[string]bool)
	if appErr := s.collectPolicyAttributes(rctx, policy, action, visited, result); appErr != nil {
		return nil, appErr
	}
	for name, values := range result {
		slices.Sort(values)
		result[name] = slices.Compact(values)
	}
	return result, nil
}

func (s *Service) collectPolicyAttributes(rctx request.CTX, policy *model.AccessControlPolicy, action string, visited map[string]bool, result map[string][]string) *model.AppError {
	if policy == nil || visited[policy.ID] {
		return nil
	}
	visited[policy.ID] = true
	for _, rule := range policy.Rules {
		if !slices.Contains(rule.Actions, action) {
			continue
		}
		attributes, err := s.engine.ruleAttributes(rule.Expression)
		if err != nil {
			return badRequest("AccessControl.GetPolicyRuleAttributes", "enterprise.access_control.invalid_expression.app_error", err.Error())
		}
		for name, values := range attributes {
			result[name] = append(result[name], values...)
		}
	}
	for _, id := range policy.Imports {
		parent, err := s.app.Srv().Store().AccessControlPolicy().Get(rctx, id)
		if err != nil {
			return storeError("AccessControl.GetPolicyRuleAttributes", "enterprise.access_control.get_policy.app_error", err)
		}
		if appErr := s.collectPolicyAttributes(rctx, parent, action, visited, result); appErr != nil {
			return appErr
		}
	}
	return nil
}

func (s *Service) OnPropertyFieldOptionsChanged(_ request.CTX, _ string) {
	// Programs are compiled per evaluation, so there is no metadata cache to invalidate.
}

func (s *Service) InvalidateAllPolicyCaches(_ request.CTX) {
	// This implementation holds no resource-policy or permission-policy cache,
	// locally or on peers. Each evaluation loads policies from the authoritative
	// store and creates its own rank resolver and CEL program. The shared engine
	// contains only the immutable language environment, not compiled policies.
	// Consequently there is no persistent state to purge or broadcast here.
}

func internalError(where, id string, err error) *model.AppError {
	return model.NewAppError(where, id, nil, err.Error(), http.StatusInternalServerError).Wrap(err)
}

func badRequest(where, id, detail string) *model.AppError {
	return model.NewAppError(where, id, nil, detail, http.StatusBadRequest)
}

func forbidden(where, id, detail string) *model.AppError {
	return model.NewAppError(where, id, nil, detail, http.StatusForbidden)
}

func storeError(where, id string, err error) *model.AppError {
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		return model.NewAppError(where, id, nil, err.Error(), http.StatusNotFound).Wrap(err)
	}
	return internalError(where, id, err)
}
