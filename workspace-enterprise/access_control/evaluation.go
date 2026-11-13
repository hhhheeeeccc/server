package access_control

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/store"
)

func (s *Service) AccessEvaluation(rctx request.CTX, accessRequest model.AccessRequest) (model.AccessDecision, *model.AppError) {
	if appErr := s.requireLicense("AccessControl.AccessEvaluation"); appErr != nil {
		return model.AccessDecision{Decision: false}, appErr
	}
	if appErr := s.ensureEngine(rctx); appErr != nil {
		return model.AccessDecision{Decision: false}, appErr
	}

	resourceAttributes, appErr := s.resourceAttributes(rctx, accessRequest.Resource)
	if appErr != nil {
		return model.AccessDecision{Decision: false}, appErr
	}

	policies, appErr := s.policiesForRequest(rctx, accessRequest)
	if appErr != nil {
		return model.AccessDecision{Decision: false}, appErr
	}
	return s.evaluateAccessRequest(rctx, accessRequest, resourceAttributes, policies, nil)
}

// Both individual and user-query evaluation use the same role, rule and guest
// decisions. A query can share policy reads, never decisions about subjects.
func (s *Service) evaluateAccessRequest(rctx request.CTX, accessRequest model.AccessRequest, resourceAttributes map[string]any, policies []*model.AccessControlPolicy, readPolicy policyReader) (model.AccessDecision, *model.AppError) {
	if len(policies) == 0 {
		return model.NewNoPolicyAccessDecision(), nil
	}

	evaluated := false
	for _, policy := range policies {
		matched, applies, err := s.evaluatePolicyGraph(rctx, policy, accessRequest, resourceAttributes, make(map[string]bool), nil, readPolicy)
		if err != nil {
			return model.AccessDecision{Decision: false}, internalError("AccessControl.AccessEvaluation", "enterprise.access_control.evaluate.app_error", err)
		}
		if !applies {
			continue
		}
		evaluated = true
		if !matched {
			return model.AccessDecision{Decision: false}, nil
		}
	}
	if !evaluated {
		return model.NewNoPolicyAccessDecision(), nil
	}
	excluded, appErr := s.privateGuestMembership(rctx, accessRequest)
	if appErr != nil {
		return model.AccessDecision{Decision: false}, appErr
	}
	return model.AccessDecision{Decision: !excluded}, nil
}

// Private membership policies exclude guests even if their attributes match.
// Public recommendations and file-permission rules remain separate.
func (s *Service) privateGuestMembership(rctx request.CTX, req model.AccessRequest) (bool, *model.AppError) {
	if req.Action != model.AccessControlPolicyActionMembership ||
		req.Resource.Type != model.AccessControlPolicyTypeChannel || req.Resource.ID == "" ||
		!slices.Contains(strings.Fields(req.Subject.RoleForScope(model.AccessControlSubjectScopeSystem)), model.SystemGuestRoleId) {
		return false, nil
	}
	channel, appErr := s.app.GetChannel(rctx, req.Resource.ID)
	if appErr != nil {
		return false, appErr
	}
	return channel.Type == model.ChannelTypePrivate, nil
}

func (s *Service) policiesForRequest(rctx request.CTX, req model.AccessRequest) ([]*model.AccessControlPolicy, *model.AppError) {
	byID := make(map[string]*model.AccessControlPolicy)
	if req.Resource.ID != "" {
		policy, err := s.app.Srv().Store().AccessControlPolicy().Get(rctx, req.Resource.ID)
		if err == nil && policy.Type == req.Resource.Type {
			byID[policy.ID] = policy
		} else if err != nil {
			var notFound *store.ErrNotFound
			if !errors.As(err, &notFound) {
				return nil, internalError("AccessControl.policiesForRequest", "enterprise.access_control.get_policy.app_error", err)
			}
		}
	}

	if req.Action != model.AccessControlPolicyActionMembership {
		policies, err := s.activePermissionPolicies(rctx, req.Action)
		if err != nil {
			return nil, internalError("AccessControl.policiesForRequest", "enterprise.access_control.search_policies.app_error", err)
		}
		for _, policy := range policies {
			if policy.Active {
				byID[policy.ID] = policy
			}
		}
	}

	result := make([]*model.AccessControlPolicy, 0, len(byID))
	for _, policy := range byID {
		result = append(result, policy)
	}
	slices.SortFunc(result, func(a, b *model.AccessControlPolicy) int { return strings.Compare(a.ID, b.ID) })
	return result, nil
}

func (s *Service) evaluatePolicy(rctx request.CTX, policy *model.AccessControlPolicy, req model.AccessRequest, resourceAttributes map[string]any, visited map[string]bool) (bool, bool, error) {
	return s.evaluatePolicyWithDraft(rctx, policy, req, resourceAttributes, visited, nil)
}

// Simulation substitutes the draft throughout the import graph without saving
// it. Live evaluation uses the same evaluator with no draft substitution.
func (s *Service) evaluatePolicyWithDraft(rctx request.CTX, policy *model.AccessControlPolicy, req model.AccessRequest, resourceAttributes map[string]any, visited map[string]bool, draft *model.AccessControlPolicy) (bool, bool, error) {
	return s.evaluatePolicyGraph(rctx, policy, req, resourceAttributes, visited, draft, nil)
}

func (s *Service) evaluatePolicyGraph(rctx request.CTX, policy *model.AccessControlPolicy, req model.AccessRequest, resourceAttributes map[string]any, visited map[string]bool, draft *model.AccessControlPolicy, readPolicy policyReader) (bool, bool, error) {
	if policy != nil && draft != nil && draft.ID != "" && policy.ID == draft.ID {
		policy = draft
	}
	if policy == nil || visited[policy.ID] {
		return true, false, nil
	}
	visited[policy.ID] = true

	if len(policy.Roles) > 0 && !subjectHasAnyRole(req.Subject, policy.Roles) {
		return true, false, nil
	}

	localApplies := false
	localAllowed := false
	for _, rule := range policy.Rules {
		if rule.IsMetadataCarrier() {
			continue
		}
		if !slices.Contains(rule.Actions, req.Action) || !ruleAppliesToRole(req.Subject, rule.Role) {
			continue
		}
		localApplies = true
		allowed, err := s.evaluateExpression(rctx, rule.Expression, req.Subject, resourceAttributes)
		if err != nil {
			return false, true, err
		}
		// Rules for the same action and role are alternatives.
		localAllowed = localAllowed || allowed
	}
	if localApplies && !localAllowed {
		return false, true, nil
	}

	anyApplies := localApplies
	for _, importedID := range policy.Imports {
		parent := draft
		if draft == nil || draft.ID != importedID {
			var err error
			if readPolicy != nil {
				parent, err = readPolicy(importedID)
			} else {
				parent, err = s.app.Srv().Store().AccessControlPolicy().Get(rctx, importedID)
			}
			if err != nil {
				return false, true, err
			}
		}
		allowed, applies, err := s.evaluatePolicyGraph(rctx, parent, req, resourceAttributes, visited, draft, readPolicy)
		if err != nil {
			return false, true, err
		}
		anyApplies = anyApplies || applies
		if applies && !allowed {
			return false, true, nil
		}
	}
	return true, anyApplies, nil
}

func subjectHasAnyRole(subject model.Subject, roles []string) bool {
	owned := make([]string, 0, len(subject.ScopedRoles)+len(strings.Fields(subject.Role)))
	for _, role := range strings.Fields(subject.Role) {
		owned = append(owned, role)
	}
	for _, scoped := range subject.ScopedRoles {
		owned = append(owned, scoped.Role)
	}
	return slices.ContainsFunc(roles, func(target string) bool {
		return slices.ContainsFunc(owned, func(actual string) bool { return roleMatches(actual, target) })
	})
}

func ruleAppliesToRole(subject model.Subject, role string) bool {
	if role == "" {
		return true
	}
	for _, scoped := range subject.ScopedRoles {
		if roleMatches(scoped.Role, role) {
			return true
		}
	}
	return slices.ContainsFunc(strings.Fields(subject.Role), func(actual string) bool { return roleMatches(actual, role) })
}

type localRuleEvaluation struct {
	rule    model.AccessControlPolicyRule
	allowed bool
}

func (s *Service) evaluateLocalRules(rctx request.CTX, policy *model.AccessControlPolicy, req model.AccessRequest, resourceAttributes map[string]any) (bool, bool, []localRuleEvaluation, error) {
	if policy == nil || (len(policy.Roles) > 0 && !subjectHasAnyRole(req.Subject, policy.Roles)) {
		return true, false, nil, nil
	}
	results := []localRuleEvaluation{}
	allowed := false
	for _, rule := range policy.Rules {
		if !slices.Contains(rule.Actions, req.Action) || !ruleAppliesToRole(req.Subject, rule.Role) {
			continue
		}
		matched, err := s.evaluateExpression(rctx, rule.Expression, req.Subject, resourceAttributes)
		if err != nil {
			return false, true, results, err
		}
		results = append(results, localRuleEvaluation{rule: rule, allowed: matched})
		allowed = allowed || matched
	}
	if len(results) == 0 {
		return true, false, results, nil
	}
	return allowed, true, results, nil
}

func roleMatches(actual, target string) bool {
	if actual == target {
		return true
	}
	return (actual == model.SystemAdminRoleId && target == model.SystemUserRoleId) ||
		(actual == model.ChannelAdminRoleId && target == model.ChannelUserRoleId)
}

func (s *Service) resourceAttributes(rctx request.CTX, resource model.Resource) (map[string]any, *model.AppError) {
	if resource.ID == "" || resource.Type != model.AccessControlPolicyTypeChannel {
		return map[string]any{}, nil
	}
	group, appErr := s.app.GetPropertyGroup(rctx, model.AccessControlPropertyGroupName)
	if appErr != nil {
		return nil, appErr
	}
	subject, err := s.app.Srv().Store().Attributes().GetSubject(rctx, resource.ID, group.ID, model.PropertyFieldObjectTypeChannel)
	if err != nil {
		var notFound *store.ErrNotFound
		if errors.As(err, &notFound) {
			return map[string]any{}, nil
		}
		return nil, internalError("AccessControl.resourceAttributes", "enterprise.access_control.resource_attributes.app_error", err)
	}
	return subject.Attributes, nil
}

func (s *Service) QueryUsersForExpression(rctx request.CTX, expression string, opts model.SubjectSearchOptions) ([]*model.User, int64, *model.AppError) {
	if appErr := s.requireLicense("AccessControl.QueryUsersForExpression"); appErr != nil {
		return nil, 0, appErr
	}
	if appErr := s.ensureEngine(rctx); appErr != nil {
		return nil, 0, appErr
	}
	if opts.ExcludeNativeAttributes {
		var appErr *model.AppError
		expression, appErr = s.expressionWithoutNativeAttributes(rctx, expression)
		if appErr != nil {
			return nil, 0, appErr
		}
	}
	if issues := s.engine.check(expression); len(issues) > 0 {
		return nil, 0, badRequest("AccessControl.QueryUsersForExpression", "enterprise.access_control.invalid_expression.app_error", issues[0].Message)
	}
	var expressionErr *model.AppError
	expression, expressionErr = s.desugarRankExpression(rctx, expression)
	if expressionErr != nil {
		return nil, 0, expressionErr
	}

	resource := model.Resource{ID: opts.ResourceID, Type: model.AccessControlPolicyTypeChannel}
	resourceAttrs, appErr := s.resourceAttributes(rctx, resource)
	if appErr != nil {
		return nil, 0, appErr
	}
	return s.queryMatchingUsers(rctx, opts, "AccessControl.QueryUsersForExpression", func(user *model.User) (bool, *model.AppError) {
		subject, buildErr := s.app.BuildAccessControlSubject(rctx, user.Id, user.Roles, opts.ResourceID)
		if buildErr != nil {
			return false, buildErr
		}
		ok, evalErr := s.evaluateExpression(rctx, expression, *subject, resourceAttrs)
		if evalErr != nil {
			return false, internalError("AccessControl.QueryUsersForExpression", "enterprise.access_control.evaluate.app_error", evalErr)
		}
		return ok, nil
	})
}

func (s *Service) QueryUsersForResource(rctx request.CTX, resourceID, action string, opts model.SubjectSearchOptions) ([]*model.User, int64, *model.AppError) {
	if appErr := s.requireLicense("AccessControl.QueryUsersForResource"); appErr != nil {
		return nil, 0, appErr
	}
	policy, err := s.app.Srv().Store().AccessControlPolicy().Get(rctx, resourceID)
	if err != nil {
		return nil, 0, storeError("AccessControl.QueryUsersForResource", "enterprise.access_control.get_policy.app_error", err)
	}
	evaluation := &resourceQueryEvaluation{}
	return s.queryMatchingUsers(rctx, opts, "AccessControl.QueryUsersForResource", func(user *model.User) (bool, *model.AppError) {
		scopedResourceID := ""
		if policy.Type == model.AccessControlPolicyTypeChannel {
			scopedResourceID = resourceID
		}
		subject, buildErr := s.app.BuildAccessControlSubject(rctx, user.Id, user.Roles, scopedResourceID)
		if buildErr != nil {
			return false, buildErr
		}
		decision, evalErr := s.evaluateQueryResource(rctx, model.AccessRequest{Subject: *subject, Resource: model.Resource{ID: resourceID, Type: policy.Type}, Action: action}, evaluation)
		if evalErr != nil {
			return false, evalErr
		}
		return decision.Decision && !decision.IsNoPolicy(), nil
	})
}

func (s *Service) GetChannelMembersToRemove(rctx request.CTX, channelID string) ([]*model.ChannelMember, *model.AppError) {
	if appErr := s.requireLicense("AccessControl.GetChannelMembersToRemove"); appErr != nil {
		return nil, appErr
	}
	members, err := s.app.Srv().Store().Attributes().GetChannelMembersToRemove(rctx, channelID, model.SubjectSearchOptions{})
	if err != nil {
		return nil, internalError("AccessControl.GetChannelMembersToRemove", "enterprise.access_control.channel_members.app_error", err)
	}
	result := make([]*model.ChannelMember, 0)
	for _, member := range members {
		user, appErr := s.app.GetUser(rctx, member.UserId)
		if appErr != nil {
			return nil, appErr
		}
		subject, appErr := s.app.BuildAccessControlSubject(rctx, user.Id, user.Roles, channelID)
		if appErr != nil {
			return nil, appErr
		}
		decision, appErr := s.AccessEvaluation(rctx, model.AccessRequest{Subject: *subject, Resource: model.Resource{ID: channelID, Type: model.AccessControlPolicyTypeChannel}, Action: model.AccessControlPolicyActionMembership})
		if appErr != nil {
			return nil, appErr
		}
		if !decision.Decision {
			result = append(result, member)
		}
	}
	return result, nil
}

func (s *Service) GetTeamMembersToRemove(rctx request.CTX, teamID string) ([]*model.TeamMember, *model.AppError) {
	if appErr := s.requireLicense("AccessControl.GetTeamMembersToRemove"); appErr != nil {
		return nil, appErr
	}
	members, err := s.app.Srv().Store().Attributes().GetTeamMembersToRemove(rctx, teamID, model.SubjectSearchOptions{})
	if err != nil {
		return nil, internalError("AccessControl.GetTeamMembersToRemove", "enterprise.access_control.team_members.app_error", err)
	}
	result := make([]*model.TeamMember, 0)
	for _, member := range members {
		user, appErr := s.app.GetUser(rctx, member.UserId)
		if appErr != nil {
			return nil, appErr
		}
		subject, appErr := s.app.BuildAccessControlSubject(rctx, user.Id, user.Roles, "")
		if appErr != nil {
			return nil, appErr
		}
		decision, appErr := s.AccessEvaluation(rctx, model.AccessRequest{Subject: *subject, Resource: model.Resource{ID: teamID, Type: model.AccessControlPolicyTypeTeam}, Action: model.AccessControlPolicyActionMembership})
		if appErr != nil {
			return nil, appErr
		}
		if !decision.Decision {
			result = append(result, member)
		}
	}
	return result, nil
}

func (s *Service) SimulatePolicyForUsers(rctx request.CTX, params model.PolicySimulationByUsersParams) (*model.PolicySimulationResponse, *model.AppError) {
	if appErr := s.requireLicense("AccessControl.SimulatePolicyForUsers"); appErr != nil {
		return nil, appErr
	}
	if params.Policy == nil || len(params.Actions) == 0 {
		return nil, badRequest("AccessControl.SimulatePolicyForUsers", "enterprise.access_control.simulation_request.app_error", "policy and actions are required")
	}
	resourceAttrs, appErr := s.resourceAttributes(rctx, model.Resource{ID: params.ChannelID, Type: model.AccessControlPolicyTypeChannel})
	if appErr != nil {
		return nil, appErr
	}
	draft := clonePolicy(params.Policy)
	if params.EvaluationScope == "" {
		params.EvaluationScope = model.PolicyEvaluationScopeThisRule
	}
	if params.EvaluationScope != model.PolicyEvaluationScopeThisRule && params.EvaluationScope != model.PolicyEvaluationScopeAll {
		return nil, badRequest("AccessControl.SimulatePolicyForUsers", "enterprise.access_control.simulation_scope.app_error", "invalid evaluation scope")
	}
	if params.EvaluationScope == model.PolicyEvaluationScopeThisRule && params.RuleName != "" {
		filtered := draft.Rules[:0]
		for _, rule := range draft.Rules {
			if rule.Name == params.RuleName {
				filtered = append(filtered, rule)
			}
		}
		draft.Rules = filtered
		draft.Imports = nil
	}
	for i, rule := range draft.Rules {
		storedExpression, appErr := s.desugarRankExpression(rctx, rule.Expression)
		if appErr != nil {
			return nil, appErr
		}
		draft.Rules[i].Expression = storedExpression
	}
	response := &model.PolicySimulationResponse{Results: []model.PolicySimulationUserResult{}, Total: int64(len(params.Users))}
	for _, input := range params.Users {
		user, appErr := s.app.GetUser(rctx, input.UserID)
		if appErr != nil {
			return nil, appErr
		}
		subject, appErr := s.app.BuildAccessControlSubject(rctx, user.Id, user.Roles, params.ChannelID)
		if appErr != nil {
			return nil, appErr
		}
		sessions, appErr := s.simulationSessions(rctx, input)
		if appErr != nil {
			return nil, appErr
		}
		row := model.PolicySimulationUserResult{User: user, Attributes: stringAttributes(subject.Attributes)}
		for _, session := range sessions {
			subject.Session = session.attributes
			decisions, appErr := s.simulationDecisions(rctx, params, draft, *subject, resourceAttrs)
			if appErr != nil {
				return nil, appErr
			}
			session.result.Decisions = decisions
			row.Sessions = append(row.Sessions, session.result)
			if row.Decisions == nil {
				// Sessions are ordered by last activity. The headline must never
				// borrow a passing result from another, older device.
				row.Decisions = decisions
			}
		}
		response.Results = append(response.Results, row)
	}
	return response, nil
}

func (s *Service) simulateAction(rctx request.CTX, params model.PolicySimulationByUsersParams, draft *model.AccessControlPolicy, req model.AccessRequest, resourceAttributes map[string]any) (model.PolicySimulationActionDecision, error) {
	draftAllowed, draftApplies, ruleResults, err := s.evaluateLocalRules(rctx, draft, req, resourceAttributes)
	if err != nil {
		return model.PolicySimulationActionDecision{}, err
	}
	decision := model.PolicySimulationActionDecision{Decision: true}
	anyApplies := draftApplies
	if draftApplies && !draftAllowed {
		decision.Decision = false
		for _, result := range ruleResults {
			if result.allowed {
				continue
			}
			source := model.PolicySimulationBlameSourceSiblingRule
			if params.RuleName == "" || result.rule.Name == params.RuleName {
				source = model.PolicySimulationBlameSourceThisRule
			}
			decision.Blame = append(decision.Blame, simulationBlame(source, model.PolicySimulationBlameOutcomeDeny, draft, result.rule, true))
		}
	}

	selectedApplies, selectedAllows := false, false
	for _, result := range ruleResults {
		if params.RuleName == "" || result.rule.Name == params.RuleName {
			selectedApplies = true
			selectedAllows = selectedAllows || result.allowed
		}
	}

	if params.EvaluationScope == model.PolicyEvaluationScopeAll {
		// Include the actual resource policy and global permission policies just
		// as live access evaluation does, even when the draft is a parent policy.
		policies, appErr := s.policiesForRequest(rctx, req)
		if appErr != nil {
			return model.PolicySimulationActionDecision{}, appErr
		}
		for _, importedID := range draft.Imports {
			parent, getErr := s.app.Srv().Store().AccessControlPolicy().Get(rctx, importedID)
			if getErr != nil {
				return model.PolicySimulationActionDecision{}, getErr
			}
			policies = append(policies, parent)
		}
		seen := map[string]bool{}
		if draft.ID != "" {
			seen[draft.ID] = true // its local rules and imports are evaluated above
		}
		for _, policy := range policies {
			if seen[policy.ID] {
				continue
			}
			seen[policy.ID] = true
			allowed, applies, evalErr := s.evaluatePolicyWithDraft(rctx, policy, req, resourceAttributes, make(map[string]bool), draft)
			if evalErr != nil {
				return model.PolicySimulationActionDecision{}, evalErr
			}
			anyApplies = anyApplies || applies
			if applies && !allowed {
				decision.Decision = false
				source := model.PolicySimulationBlameSourceChannelPolicy
				if policy.Type == model.AccessControlPolicyTypePermission {
					source = model.PolicySimulationBlameSourceSystemPermission
				}
				decision.Blame = append(decision.Blame, model.PolicySimulationBlame{
					Source: source, Outcome: model.PolicySimulationBlameOutcomeDeny,
					PolicyID: policy.ID, PolicyName: policy.Name,
				})
			}
		}
		if anyApplies {
			excluded, appErr := s.privateGuestMembership(rctx, req)
			if appErr != nil {
				return model.PolicySimulationActionDecision{}, appErr
			}
			if excluded {
				decision.Decision = false
				decision.Blame = append(decision.Blame, model.PolicySimulationBlame{
					Source: model.PolicySimulationBlameSourceChannelPolicy, Outcome: model.PolicySimulationBlameOutcomeDeny,
					PolicyID: req.Resource.ID,
				})
			}
		}
	}

	if !anyApplies {
		source := model.PolicySimulationBlameSourceNoApplicablePolicy
		if params.EvaluationScope == model.PolicyEvaluationScopeThisRule {
			source = model.PolicySimulationBlameSourceNoApplicableRule
		}
		decision.Decision = true
		decision.Blame = []model.PolicySimulationBlame{{Source: source}}
		return decision, nil
	}

	if decision.Decision && selectedApplies {
		if selectedAllows {
			var selectedRule model.AccessControlPolicyRule
			for _, result := range ruleResults {
				if (params.RuleName == "" || result.rule.Name == params.RuleName) && result.allowed {
					selectedRule = result.rule
					break
				}
			}
			decision.Blame = append(decision.Blame, simulationBlame(model.PolicySimulationBlameSourceThisRule, model.PolicySimulationBlameOutcomeAllow, draft, selectedRule, true))
		} else if draftAllowed {
			decision.Blame = append(decision.Blame, model.PolicySimulationBlame{Source: model.PolicySimulationBlameSourceSiblingSaved, Outcome: model.PolicySimulationBlameOutcomeAllow, PolicyID: draft.ID, PolicyName: draft.Name})
		}
	}
	if !decision.Decision && draftAllowed && selectedAllows {
		var selectedRule model.AccessControlPolicyRule
		for _, result := range ruleResults {
			if (params.RuleName == "" || result.rule.Name == params.RuleName) && result.allowed {
				selectedRule = result.rule
				break
			}
		}
		decision.Blame = append(decision.Blame, simulationBlame(model.PolicySimulationBlameSourceThisRule, model.PolicySimulationBlameOutcomeAllow, draft, selectedRule, true))
	}
	return decision, nil
}

func simulationBlame(source, outcome string, policy *model.AccessControlPolicy, rule model.AccessControlPolicyRule, exposeExpression bool) model.PolicySimulationBlame {
	blame := model.PolicySimulationBlame{
		Source: source, Outcome: outcome, PolicyID: policy.ID, PolicyName: policy.Name, RuleName: rule.Name, Role: rule.Role,
	}
	if exposeExpression {
		blame.Expression = rule.Expression
	}
	return blame
}

func (s *Service) activePermissionPolicies(rctx request.CTX, action string) ([]*model.AccessControlPolicy, error) {
	result := []*model.AccessControlPolicy{}
	cursor := model.AccessControlPolicyCursor{}
	for {
		policies, _, err := s.app.Srv().Store().AccessControlPolicy().SearchPolicies(rctx, model.AccessControlPolicySearch{
			Type: model.AccessControlPolicyTypePermission, Active: true, Actions: []string{action}, Cursor: cursor, Limit: 1000,
		})
		if err != nil {
			return nil, err
		}
		result = append(result, policies...)
		if len(policies) < 1000 {
			break
		}
		cursor.ID = policies[len(policies)-1].ID
	}
	return result, nil
}

func stringAttributes(attributes map[string]any) map[string]string {
	result := make(map[string]string, len(attributes))
	for key, value := range attributes {
		result[key] = strings.TrimSpace(toString(value))
	}
	return result
}

func toString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]any:
		// Rank fields are materialized as {name, rank}; the option name is
		// the useful value in the simulation details panel.
		if name, ok := typed["name"].(string); ok {
			return name
		}
		return fmt.Sprint(typed)
	default:
		return fmt.Sprint(value)
	}
}
