package online_users

import (
	"fmt"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

// Compare paged navigation with the native complete report, preserving the
// database's collation and the established ascending Id tiebreaker.
func TestReviewReportCursorMatrix(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) { cfg.CacheSettings.CacheType = new(model.CacheTypeLRU) }).InitBasic(t)
	service := &Service{app: th.App}
	prefix := "matrix_" + model.NewId()
	for i, name := range []string{"", "", "same", "same", "third"} {
		_, err := th.Store.User().Save(th.Context, &model.User{
			Username: fmt.Sprintf("%s_%d", prefix, i), Email: fmt.Sprintf("%s_%d@example.com", prefix, i),
			FirstName: name, LastName: name, Nickname: name, Roles: model.SystemUserRoleId,
			CreateAt: 1000 + int64(i/2),
		})
		require.NoError(t, err)
	}
	ids := func(reports []*model.UserReport) []string {
		result := make([]string, len(reports))
		for i, report := range reports {
			result[i] = report.Id
		}
		return result
	}
	for _, column := range model.UserReportSortColumns {
		for _, desc := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/descending=%t", column, desc), func(t *testing.T) {
				options := &model.UserReportOptions{
					ReportingBaseOptions: model.ReportingBaseOptions{SortColumn: column, SortDesc: desc, Direction: "next", PageSize: 100},
					SearchTerm:           prefix, Role: model.SystemUserRoleId,
				}
				full, appErr := th.App.GetUsersForReporting(options)
				require.Nil(t, appErr)
				require.Len(t, full, 5)
				options.PageSize = 2
				for start := 0; start < len(full); start += 2 {
					page, appErr := service.reportPage(th.Context.Context(), options)
					require.Nil(t, appErr)
					require.Equal(t, ids(full[start:min(start+2, len(full))]), ids(page), "forward page at offset %d", start)
					if start > 0 {
						previous := *options
						previous.Direction = "prev"
						previous.FromId = page[0].Id
						previous.FromColumnValue = reportSortValue(page[0], column)
						prevPage, appErr := service.reportPage(th.Context.Context(), &previous)
						require.Nil(t, appErr)
						require.Equal(t, ids(full[start-2:start]), ids(prevPage), "previous page at offset %d", start)
					}
					last := page[len(page)-1]
					options.FromId = last.Id
					options.FromColumnValue = reportSortValue(last, column)
				}
				end, appErr := service.reportPage(th.Context.Context(), options)
				require.Nil(t, appErr)
				require.Empty(t, end, "the final cursor must not repeat any row")
			})
		}
	}
}
