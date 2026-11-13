package legal_hold

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/stretchr/testify/require"
)

func caseAPIFixture(t *testing.T) (*api4.TestHelper, *FileCoordinator, *CaseManager, *CaseAPI, string) {
	t.Helper()
	th, c, m, _, _ := caseFixture(t)
	require.NoError(t, InstallCaseAPIAudit(context.Background(), m.journal))
	e, _ := exportFixture(t, m, 32<<20)
	a, err := NewCaseAPI(m, e)
	require.NoError(t, err)
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	return th, c, m, a, server.URL
}

func caseAPIHTTP(t *testing.T, base, token, method, path string, body any) (int, http.Header, []byte) {
	t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		data, err := json.Marshal(b)
		require.NoError(t, err)
		reader = bytes.NewReader(data)
	}
	r, err := http.NewRequest(method, base+CaseAPIPath+path, reader)
	require.NoError(t, err)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept-Language", "ar")
	response, err := http.DefaultClient.Do(r)
	require.NoError(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, response.Header, data
}

func TestCaseAPILifecycleAndAuditOverActualHTTP(t *testing.T) {
	th, c, m, a, base := caseAPIFixture(t)
	_, info := uploadFixtureFile(t, th, []byte("actual HTTP legal hold download"))
	key := model.NewId()
	input := caseAPIStart{RequestKey: key, Title: "قضية عبر HTTP", Scopes: []caseAPIScope{{"channels", th.BasicChannel.Id}}}
	code, headers, body := caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "POST", "/cases", input)
	require.Equal(t, 200, code, string(body))
	require.Equal(t, "no-store", headers.Get("Cache-Control"))
	var started caseAPIStatus
	require.NoError(t, json.Unmarshal(body, &started))
	require.Equal(t, "active", started.State)
	code, _, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "POST", "/cases", input)
	require.Equal(t, 200, code, string(body))
	var replay caseAPIStatus
	require.NoError(t, json.Unmarshal(body, &replay))
	require.Equal(t, started, replay)
	code, _, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "GET", "/cases?limit=1", nil)
	require.Equal(t, 200, code, string(body))
	require.Contains(t, string(body), started.ID)
	code, _, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "GET", "/cases/"+started.ID, nil)
	require.Equal(t, 200, code, string(body))
	require.Contains(t, string(body), "قضية عبر HTTP")
	code, _, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "POST", "/cases/"+started.ID+"/resume", struct{}{})
	require.Equal(t, 200, code, string(body))
	other := th.CreatePublicChannel(t)
	change := caseAPIChange{RequestKey: model.NewId(), Reason: "توسيع نطاق القضية", Add: []caseAPIScope{{"channels", other.Id}}}
	code, _, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "POST", "/cases/"+started.ID+"/scope", change)
	require.Equal(t, 200, code, string(body))
	var changed caseAPIScopeStatus
	require.NoError(t, json.Unmarshal(body, &changed))
	code, _, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "POST", "/scope_requests/"+changed.ID+"/resume", struct{}{})
	require.Equal(t, 200, code, string(body))
	code, _, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "POST", "/cases/"+started.ID+"/exports", caseAPIKey{model.NewId()})
	require.Equal(t, 200, code, string(body))
	var exported caseAPIExportStatus
	require.NoError(t, json.Unmarshal(body, &exported))
	require.Equal(t, "complete", exported.State)
	for _, path := range []string{"/exports/" + exported.ID, "/cases/" + started.ID + "/exports?limit=1"} {
		code, _, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "GET", path, nil)
		require.Equal(t, 200, code, string(body))
		require.Contains(t, string(body), exported.ID)
	}
	code, _, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "POST", "/exports/"+exported.ID+"/resume", struct{}{})
	require.Equal(t, 200, code, string(body))
	code, headers, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "GET", "/exports/"+exported.ID+"/download", nil)
	require.Equal(t, 200, code)
	require.Equal(t, "application/zip", headers.Get("Content-Type"))
	require.Contains(t, headers.Get("Content-Disposition"), exported.ID+".zip")
	_, err := VerifyCaseExport(context.Background(), bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err)
	var downloaded int64
	var actor, outcome string
	require.NoError(t, c.fence.db.QueryRow(`SELECT o.bytes_written,i.actor_id,o.outcome FROM workspace_legal_hold.api_results o JOIN workspace_legal_hold.api_identities i USING(request_id) WHERE o.request_id=$1`, headers.Get("X-Legal-Hold-Request-Id")).Scan(&downloaded, &actor, &outcome))
	require.Equal(t, int64(len(body)), downloaded)
	require.Equal(t, th.SystemAdminUser.Id, actor)
	require.Equal(t, "succeeded", outcome)
	code, _, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "GET", "/audit?case_id="+started.ID+"&limit=100", nil)
	require.Equal(t, 200, code, string(body))
	require.Contains(t, string(body), "start_case")
	require.Contains(t, string(body), "download")
	require.NotContains(t, string(body), th.SystemAdminClient.AuthToken)
	require.NotContains(t, string(body), "actual HTTP legal hold download")
	require.NotContains(t, string(body), info.Path)
	code, _, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "POST", "/cases/"+started.ID+"/release", caseAPIRelease{model.NewId(), "انتهاء القضية"})
	require.Equal(t, 200, code, string(body))
	var released caseAPIReleaseStatus
	require.NoError(t, json.Unmarshal(body, &released))
	require.Equal(t, "released", released.State)
	details, err := m.Get(th.Context.WithSession(&model.Session{UserId: th.SystemAdminUser.Id}), started.ID)
	require.NoError(t, err)
	require.Equal(t, released.ExportID, details.FinalExportID)
	require.Same(t, m, a.exporter.manager)
}

func TestCaseAPIAuthenticationValidationAndLanguage(t *testing.T) {
	th, c, _, _, base := caseAPIFixture(t)
	input := caseAPIStart{RequestKey: model.NewId(), Title: "صلاحيات API", Scopes: []caseAPIScope{{"channels", th.BasicChannel.Id}}}
	for name, token := range map[string]string{"anonymous": "", "regular": th.Client.AuthToken} {
		t.Run(name, func(t *testing.T) {
			code, headers, body := caseAPIHTTP(t, base, token, "POST", "/cases", input)
			if token == "" {
				require.Equal(t, 401, code, string(body))
			} else {
				require.Equal(t, 403, code, string(body))
			}
			require.Equal(t, "ar", headers.Get("Content-Language"))
			require.NotContains(t, string(body), "pq:")
		})
	}
	th.Server.SetLicense(nil)
	code, _, body := caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "POST", "/cases", input)
	require.Equal(t, 403, code, string(body))
	th.Server.SetLicense(model.NewTestLicense("compliance"))
	for name, bad := range map[string]string{"null": "null", "array": "[]", "unknown-field": `{"request_key":"` + input.RequestKey + `","Title":"ignore me","roles":"system_admin"}`, "multiple": "{} {}", "large": `{"title":"` + strings.Repeat("x", caseAPIMaxBody) + `"}`} {
		t.Run(name, func(t *testing.T) {
			code, _, body := caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "POST", "/cases", bad)
			if name == "large" {
				require.Equal(t, 413, code, string(body))
			} else {
				require.Equal(t, 400, code, string(body))
			}
		})
	}
	for _, path := range []string{"/cases?limit=0", "/cases?limit=101", "/cases?limit=2&limit=3", "/cases?password=secret", "/cases/not-an-id", "/audit?after=invalid", "/audit?case_id=bad"} {
		code, _, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "GET", path, nil)
		require.Equal(t, 400, code, string(body))
	}
	code, _, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "DELETE", "/cases", nil)
	require.Equal(t, 405, code, string(body))
	var count int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.cases`).Scan(&count))
	require.Zero(t, count)
	r, err := http.NewRequest("GET", base+CaseAPIPath+"/cases/invalid", nil)
	require.NoError(t, err)
	r.Header.Set("Authorization", "Bearer "+th.SystemAdminClient.AuthToken)
	r.Header.Set("Accept-Language", "en")
	res, err := http.DefaultClient.Do(r)
	require.NoError(t, err)
	defer res.Body.Close()
	body, err = io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equal(t, "en", res.Header.Get("Content-Language"))
	require.Contains(t, string(body), "Invalid request")
}

func TestCaseAPIAuditFailureDoesNotAnnounceSuccessAndRetryRecovers(t *testing.T) {
	th, c, _, _, base := caseAPIFixture(t)
	input := caseAPIStart{RequestKey: model.NewId(), Title: "استعادة رد التدقيق", Scopes: []caseAPIScope{{"channels", th.BasicChannel.Id}}}
	_, err := c.fence.db.Exec(`CREATE FUNCTION public.reject_legal_audit_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'private audit failure detail'; END $$;`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := c.fence.db.Exec(`DROP FUNCTION IF EXISTS public.reject_legal_audit_fixture() CASCADE`)
		require.NoError(t, err)
	})
	for _, table := range []string{"api_requests", "api_identities", "api_actions", "api_results"} {
		t.Run(table, func(t *testing.T) {
			_, err := c.fence.db.Exec("CREATE TRIGGER fixture_reject BEFORE INSERT ON workspace_legal_hold." + table + " FOR EACH ROW EXECUTE FUNCTION public.reject_legal_audit_fixture()")
			require.NoError(t, err)
			code, _, body := caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "POST", "/cases", input)
			require.Equal(t, 503, code, string(body))
			require.NotContains(t, string(body), "private audit failure detail")
			require.NotContains(t, string(body), `"state":"active"`)
			_, err = c.fence.db.Exec("DROP TRIGGER fixture_reject ON workspace_legal_hold." + table)
			require.NoError(t, err)
			var count int
			require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.cases`).Scan(&count))
			if table == "api_results" {
				require.Equal(t, 1, count, "the domain commit precedes the missing audit result")
			} else {
				require.Zero(t, count, "no business action without durable audit admission")
			}
		})
	}
	code, _, body := caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "POST", "/cases", input)
	require.Equal(t, 200, code, string(body))
	var count int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.cases`).Scan(&count))
	require.Equal(t, 1, count, "lost final audit response cannot duplicate the case")
	var unknown int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.api_requests r JOIN workspace_legal_hold.api_actions a ON a.request_id=r.id LEFT JOIN workspace_legal_hold.api_results o ON o.request_id=r.id WHERE o.request_id IS NULL`).Scan(&unknown))
	require.Equal(t, 1, unknown)
}
