package access_control

import (
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

const userEvaluationPageSize = 100

func (s *Service) queryMatchingUsers(rctx request.CTX, opts model.SubjectSearchOptions, where string, matches func(*model.User) (bool, *model.AppError)) ([]*model.User, int64, *model.AppError) {
	return scanMatchingUsers(rctx, opts, where, func(page model.SubjectSearchOptions) ([]*model.User, error) {
		users, _, err := s.app.Srv().Store().Attributes().SearchUsers(rctx, page)
		return users, err
	}, matches)
}

// SearchUsers orders by user ID. Scan bounded candidate pages, then apply the
// full CEL/policy evaluator; translating an arbitrary policy to SQL could lose
// its permissions semantics. Exact counts still cover matches before the
// requested cursor, while IgnoreCount can start at the cursor and stop early.
func scanMatchingUsers(rctx request.CTX, opts model.SubjectSearchOptions, where string, search func(model.SubjectSearchOptions) ([]*model.User, error), matches func(*model.User) (bool, *model.AppError)) ([]*model.User, int64, *model.AppError) {
	page := opts
	page.Query, page.Args = "", nil
	page.Limit, page.IgnoreCount = userEvaluationPageSize, true
	page.Cursor = model.SubjectCursor{}
	if opts.IgnoreCount {
		page.Cursor.TargetID = opts.Cursor.TargetID
	}
	matched := make([]*model.User, 0)
	var total int64
	for {
		if err := rctx.Context().Err(); err != nil {
			return nil, 0, internalError(where, "enterprise.access_control.search_users.app_error", err)
		}
		users, err := search(page)
		if err != nil {
			return nil, 0, internalError(where, "enterprise.access_control.search_users.app_error", err)
		}
		for _, user := range users {
			if err := rctx.Context().Err(); err != nil {
				return nil, 0, internalError(where, "enterprise.access_control.evaluate.app_error", err)
			}
			ok, appErr := matches(user)
			if appErr != nil {
				return nil, 0, appErr
			}
			if !ok {
				continue
			}
			if !opts.IgnoreCount {
				total++
			}
			if user.Id <= opts.Cursor.TargetID {
				continue
			}
			if opts.Limit <= 0 || len(matched) < opts.Limit {
				matched = append(matched, user)
			}
			if opts.IgnoreCount && opts.Limit > 0 && len(matched) == opts.Limit {
				return matched, 0, nil
			}
		}
		if len(users) < page.Limit {
			return matched, total, nil
		}
		page.Cursor.TargetID = users[len(users)-1].Id
	}
}
