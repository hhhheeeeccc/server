package team_deletion_test

import (
	"context"
	"net/http"
	"testing"

	_ "github.com/mattermost/enterprise"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/stretchr/testify/require"
)

func TestTeamDeletionRouteLicensePermissionAndMode(t *testing.T) {
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		cfg.ServiceSettings.EnableAPITeamDeletion = model.NewPointer(true)
	}).InitBasic(t)
	ctx := context.Background()
	createTeam := func() *model.Team {
		id := model.NewId()
		team, _, err := th.SystemAdminClient.CreateTeam(ctx, &model.Team{
			DisplayName: "Deletion test " + id[:8],
			Name:        "delete" + id[:18],
			Email:       "delete-" + id + "@example.com",
			Type:        model.TeamOpen,
		})
		require.NoError(t, err)
		return team
	}

	team := createTeam()
	response, err := th.SystemAdminClient.PermanentDeleteTeam(ctx, team.Id)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode, "Enterprise permanent deletion must require a license")
	_, appErr := th.App.GetTeam(team.Id)
	require.Nil(t, appErr)

	th.App.Srv().SetLicense(model.NewTestLicense())
	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.ServiceSettings.EnableAPITeamDeletion = model.NewPointer(false)
	})
	response, err = th.SystemAdminClient.PermanentDeleteTeam(ctx, team.Id)
	require.Error(t, err)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	_, appErr = th.App.GetTeam(team.Id)
	require.Nil(t, appErr)

	th.App.UpdateConfig(func(cfg *model.Config) {
		cfg.ServiceSettings.EnableAPITeamDeletion = model.NewPointer(true)
	})
	response, err = th.Client.PermanentDeleteTeam(ctx, team.Id)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	_, appErr = th.App.GetTeam(team.Id)
	require.Nil(t, appErr)

	_, err = th.SystemAdminClient.PermanentDeleteTeam(ctx, team.Id)
	require.NoError(t, err)
	_, appErr = th.App.GetTeam(team.Id)
	require.NotNil(t, appErr)

	th.App.Srv().SetLicense(nil)
	softTeam := createTeam()
	response, err = th.Client.SoftDeleteTeam(ctx, softTeam.Id)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	active, appErr := th.App.GetTeam(softTeam.Id)
	require.Nil(t, appErr)
	require.Zero(t, active.DeleteAt)
	_, err = th.SystemAdminClient.SoftDeleteTeam(ctx, softTeam.Id)
	require.NoError(t, err, "ordinary soft deletion remains available without Enterprise")
	archived, appErr := th.App.GetTeam(softTeam.Id)
	require.Nil(t, appErr)
	require.NotZero(t, archived.DeleteAt)
}

func TestMain(m *testing.M) {
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}
