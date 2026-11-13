package access_control

import (
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

type policyReader func(string) (*model.AccessControlPolicy, error)

// One instance belongs to one QueryUsersForResource invocation, whose resource
// and action are fixed. Nothing is retained across requests or service calls.
// Imported policies remain lazy, so an unused import cannot fail a query.
type resourceQueryEvaluation struct {
	loaded             bool
	resourceAttributes map[string]any
	policies           []*model.AccessControlPolicy
	imports            map[string]*model.AccessControlPolicy
}

func (s *Service) evaluateQueryResource(rctx request.CTX, req model.AccessRequest, evaluation *resourceQueryEvaluation) (model.AccessDecision, *model.AppError) {
	// Keep the per-subject license and engine checks from AccessEvaluation.
	if appErr := s.requireLicense("AccessControl.AccessEvaluation"); appErr != nil {
		return model.AccessDecision{Decision: false}, appErr
	}
	if appErr := s.ensureEngine(rctx); appErr != nil {
		return model.AccessDecision{Decision: false}, appErr
	}
	if !evaluation.loaded {
		attributes, appErr := s.resourceAttributes(rctx, req.Resource)
		if appErr != nil {
			return model.AccessDecision{Decision: false}, appErr
		}
		policies, appErr := s.policiesForRequest(rctx, req)
		if appErr != nil {
			return model.AccessDecision{Decision: false}, appErr
		}
		evaluation.resourceAttributes = attributes
		evaluation.policies = policies
		evaluation.imports = make(map[string]*model.AccessControlPolicy)
		evaluation.loaded = true
	}
	readPolicy := func(id string) (*model.AccessControlPolicy, error) {
		if policy, ok := evaluation.imports[id]; ok {
			return policy, nil
		}
		policy, err := s.app.Srv().Store().AccessControlPolicy().Get(rctx, id)
		if err == nil {
			evaluation.imports[id] = policy
		}
		return policy, err
	}
	return s.evaluateAccessRequest(rctx, req, evaluation.resourceAttributes, evaluation.policies, readPolicy)
}
