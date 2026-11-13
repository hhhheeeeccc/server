package access_control_job

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/mattermost/enterprise/access_control"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func setupChannelAttributeHTTP(t *testing.T) (*api4.TestHelper, *Worker) {
	t.Helper()
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		*cfg.AccessControlSettings.EnableAttributeBasedAccessControl = true
		cfg.FeatureFlags.ChannelAttributes = true
		cfg.FeatureFlags.ResourceAttributesInPolicies = true
		cfg.FeatureFlags.ChannelPermissionPolicies = true
		cfg.FeatureFlags.PermissionPolicies = true
		*cfg.EmailSettings.SendEmailNotifications = false
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)))
	th.Server.Channels().AccessControl = access_control.New(th.App)
	worker := NewChannel(th.Server).MakeWorker().(*Worker)
	th.Server.Jobs.RegisterJobType(model.JobTypeAccessControlSync, worker, nil)
	return th, worker
}

// Exercise the actual HTTP handlers and filestore with a non-admin account,
// including requests after an attribute changes. Simulation
// alone does not establish that the upload/download and membership APIs enforce
// the same policy.
func TestChannelAttributeHTTPEnforcementEndToEnd(t *testing.T) {
	th, worker := setupChannelAttributeHTTP(t)
	ctx := th.Context.Context()
	admin := th.SystemAdminClient
	member := th.CreateClient()
	_, _, err := member.Login(ctx, th.BasicUser2.Email, th.BasicUser2.Password)
	require.NoError(t, err)
	require.False(t, th.BasicUser2.IsSystemAdmin())
	channel := th.CreateChannelWithClient(t, admin, model.ChannelTypePrivate)
	_, _, err = admin.AddChannelMember(ctx, channel.Id, th.BasicUser2.Id)
	require.NoError(t, err)
	cm, _, err := member.GetChannelMember(ctx, channel.Id, th.BasicUser2.Id, "")
	require.NoError(t, err)
	require.Equal(t, model.ChannelUserRoleId, cm.Roles)

	field, _, err := admin.CreatePropertyField(ctx, model.AccessControlPropertyGroupName, "channel", &model.PropertyField{
		Name: "department_" + model.NewId(), Type: model.PropertyFieldTypeText,
		ObjectType: "channel", TargetType: "system",
		PermissionField: new(model.PermissionLevelSysadmin), PermissionValues: new(model.PermissionLevelAdmin),
		PermissionOptions: new(model.PermissionLevelSysadmin),
	})
	require.NoError(t, err)
	writeValue := func(value json.RawMessage) {
		t.Helper()
		values, _, err := admin.PatchPropertyValues(ctx, model.AccessControlPropertyGroupName, "channel", channel.Id,
			[]model.PropertyValuePatchItem{{FieldID: field.ID, Value: value}})
		require.NoError(t, err)
		require.Len(t, values, 1)
	}
	writeValue(json.RawMessage(`"Engineering"`))
	_, response, err := member.PatchPropertyValues(ctx, model.AccessControlPropertyGroupName, "channel", channel.Id,
		[]model.PropertyValuePatchItem{{FieldID: field.ID, Value: json.RawMessage(`"Legal"`)}})
	require.Error(t, err, "a regular member must not change a controlled channel attribute")
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	_, response, err = member.PatchPropertyField(ctx, model.AccessControlPropertyGroupName, "channel", field.ID,
		&model.PropertyFieldPatch{Name: new("unauthorized")})
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	uploaded, _, err := admin.UploadFile(ctx, []byte("ABAC isolated HTTP fixture\n"), channel.Id, "abac-proof.txt")
	require.NoError(t, err)
	require.Len(t, uploaded.FileInfos, 1)
	fileID := uploaded.FileInfos[0].Id
	_, _, err = admin.CreatePost(ctx, &model.Post{ChannelId: channel.Id, Message: "Isolated ABAC fixture", FileIds: []string{fileID}})
	require.NoError(t, err)
	fileRule := fmt.Sprintf("resource.attributes.%s == 'Engineering'", field.Name)
	policy, _, err := admin.CreateAccessControlPolicy(ctx, &model.AccessControlPolicy{
		ID: channel.Id, Type: model.AccessControlPolicyTypeChannel,
		Rules: []model.AccessControlPolicyRule{{Name: "files", Role: model.ChannelUserRoleId,
			Actions: []string{model.AccessControlPolicyActionUploadFileAttachment, model.AccessControlPolicyActionDownloadFileAttachment}, Expression: fileRule}},
	})
	require.NoError(t, err)
	checkFiles := func(allowed bool) {
		t.Helper()
		// The public server refreshes ChannelAttributeView on a 30-second
		// cadence. Verify convergence through real requests, not by manually
		// refreshing the store or assuming immediate consistency.
		start := time.Now()
		var data []byte
		var response *model.Response
		var err error
		require.Eventually(t, func() bool {
			data, response, err = member.GetFile(ctx, fileID)
			if allowed {
				return err == nil
			}
			return err != nil && response != nil && response.StatusCode == http.StatusForbidden
		}, 40*time.Second, time.Second, "file permission must converge after the public server's attribute-view refresh")
		t.Logf("file download allowed=%v converged in %s", allowed, time.Since(start).Round(time.Millisecond))
		if allowed {
			require.NoError(t, err)
			require.Equal(t, "ABAC isolated HTTP fixture\n", string(data))
		} else {
			require.Error(t, err)
			require.Equal(t, http.StatusForbidden, response.StatusCode)
		}
		_, response, err = member.UploadFile(ctx, []byte("regular member upload"), channel.Id, "member-proof.txt")
		if allowed {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
			require.Equal(t, http.StatusForbidden, response.StatusCode)
		}
	}
	checkFiles(true)
	writeValue(json.RawMessage(`"Legal"`))
	checkFiles(false)
	writeValue(json.RawMessage(`"Engineering"`))
	checkFiles(true)
	writeValue(json.RawMessage(`null`))
	checkFiles(false)
	writeValue(json.RawMessage(`"Engineering"`))
	checkFiles(true)

	userField := createAttribute(t, th, &model.PropertyField{
		Type: model.PropertyFieldTypeText, Attrs: model.StringInterface{model.PropertyFieldAttrManaged: "admin"},
	}, th.BasicUser2.Id, "Engineering")
	policy.Rules = []model.AccessControlPolicyRule{{Name: "membership", Actions: []string{model.AccessControlPolicyActionMembership},
		Expression: fmt.Sprintf("user.attributes.%s == resource.attributes.%s", userField.Name, field.Name)}}
	policy.SetAutoAddMode(model.AccessControlAutoAddAlways)
	_, _, err = admin.CreateAccessControlPolicy(ctx, policy)
	require.NoError(t, err)
	sync := func() {
		t.Helper()
		job, appErr := th.Server.Jobs.CreateJob(th.Context, model.JobTypeAccessControlSync, map[string]string{"channel_ids": channel.Id})
		require.Nil(t, appErr)
		require.False(t, worker.doJob(job))
		stored, err := th.Store.Job().Get(th.Context, job.Id)
		require.NoError(t, err)
		require.Equal(t, model.JobStatusSuccess, stored.Status, stored.Data)
	}
	sync()
	requireMembership(t, th, channel.Id, th.BasicUser2.Id, true)
	writeValue(json.RawMessage(`"Legal"`))
	sync()
	requireMembership(t, th, channel.Id, th.BasicUser2.Id, false)
	_, response, err = member.GetPostsForChannel(ctx, channel.Id, 0, 10, "", false, false)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	_, response, err = admin.AddChannelMember(ctx, channel.Id, th.BasicUser2.Id)
	require.Error(t, err, "manual admission must not bypass a nonmatching membership rule")
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	writeValue(json.RawMessage(`"Engineering"`))
	sync()
	requireMembership(t, th, channel.Id, th.BasicUser2.Id, true)
	_, _, err = member.GetPostsForChannel(ctx, channel.Id, 0, 10, "", false, false)
	require.NoError(t, err)
}
