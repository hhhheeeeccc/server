package access_control

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/stretchr/testify/require"
)

func TestScanMatchingUsersPreservesPaginationAndCounts(t *testing.T) {
	users := make([]*model.User, 451)
	for i := range users {
		users[i] = &model.User{Id: fmt.Sprintf("%026d", i+1), EmailVerified: i%7 == 0}
	}
	for _, ignoreCount := range []bool{false, true} {
		for _, limit := range []int{-1, 0, 1, 15, 1000} {
			for _, cursor := range []string{"", users[0].Id, users[212].Id, users[450].Id} {
				t.Run(fmt.Sprintf("count=%v/limit=%d/cursor=%s", !ignoreCount, limit, cursor), func(t *testing.T) {
					opts := model.SubjectSearchOptions{
						Limit: limit, IgnoreCount: ignoreCount, Cursor: model.SubjectCursor{TargetID: cursor},
						TeamID: "team", Term: "name", ExcludeChannelMembers: "channel", AllowInactive: true,
						Query: "untrusted SQL", Args: []any{"unused"},
					}
					var candidates, evaluations int
					found, total, appErr := scanMatchingUsers(request.TestContext(t), opts, "test", func(page model.SubjectSearchOptions) ([]*model.User, error) {
						require.Positive(t, page.Limit)
						require.LessOrEqual(t, page.Limit, userEvaluationPageSize)
						require.True(t, page.IgnoreCount, "the SQL count is not the CEL match count")
						require.Empty(t, page.Query)
						require.Nil(t, page.Args)
						require.Equal(t, opts.TeamID, page.TeamID)
						require.Equal(t, opts.Term, page.Term)
						require.Equal(t, opts.ExcludeChannelMembers, page.ExcludeChannelMembers)
						require.True(t, page.AllowInactive)
						var batch []*model.User
						for _, user := range users {
							if user.Id > page.Cursor.TargetID && len(batch) < page.Limit {
								batch = append(batch, user)
							}
						}
						candidates += len(batch)
						return batch, nil
					}, func(user *model.User) (bool, *model.AppError) {
						evaluations++
						return user.EmailVerified, nil
					})
					require.Nil(t, appErr)
					var expected []string
					var expectedCount int64
					for _, user := range users {
						if !user.EmailVerified {
							continue
						}
						if !ignoreCount {
							expectedCount++
						}
						if user.Id > cursor && (limit <= 0 || len(expected) < limit) {
							expected = append(expected, user.Id)
						}
					}
					var actual []string
					for _, user := range found {
						actual = append(actual, user.Id)
					}
					require.Equal(t, expected, actual)
					require.Equal(t, expectedCount, total)
					if ignoreCount && limit == 1 && cursor == "" {
						require.Equal(t, 1, evaluations, "stop policy evaluation once the requested page is full")
						require.Less(t, candidates, len(users), "do not materialize every user for a one-result request")
					}
					if !ignoreCount {
						require.Equal(t, len(users), evaluations, "exact counts include matches before the cursor")
					}
				})
			}
		}
	}
}

func TestScanMatchingUsersStopsOnCancellationOrFailure(t *testing.T) {
	for _, failure := range []string{"cancel", "search", "evaluate"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rctx := request.TestContext(t).WithContext(ctx)
			var evaluated int
			found, total, appErr := scanMatchingUsers(rctx, model.SubjectSearchOptions{}, "test", func(model.SubjectSearchOptions) ([]*model.User, error) {
				if failure == "search" {
					return nil, errors.New("store failed")
				}
				return []*model.User{{Id: "a"}, {Id: "b"}}, nil
			}, func(*model.User) (bool, *model.AppError) {
				evaluated++
				if failure == "evaluate" {
					return false, internalError("test", "evaluation failed", errors.New("invalid policy"))
				}
				cancel()
				return true, nil
			})
			require.NotNil(t, appErr)
			require.Nil(t, found, "a failed scan must not return a partial authorization result")
			require.Zero(t, total)
			require.LessOrEqual(t, evaluated, 1)
		})
	}
}
