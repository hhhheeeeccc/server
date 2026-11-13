package legal_hold

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestCaseAPIPendingPreparationAndScopeResume(t *testing.T) {
	th, c, _, _, base := caseAPIFixture(t)
	_, info := uploadFixtureFile(t, th, []byte("pending HTTP preparation"))
	preview := info.Path + ".preview"
	_, err := c.fence.db.Exec(`UPDATE fileinfo SET previewpath=$2 WHERE id=$1`, info.Id, preview)
	require.NoError(t, err)
	input := caseAPIStart{model.NewId(), "قضية معلقة", []caseAPIScope{{"channels", th.BasicChannel.Id}}}
	code, _, body := caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "POST", "/cases", input)
	require.Equal(t, 202, code, string(body))
	var pending struct {
		Data caseAPIStatus `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &pending))
	require.Equal(t, "preparing", pending.Data.State)
	_, err = c.source.WriteFile(bytes.NewReader([]byte("restored preview")), preview)
	require.NoError(t, err)
	code, _, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "POST", "/cases", input)
	require.Equal(t, 200, code, string(body))
	var active caseAPIStatus
	require.NoError(t, json.Unmarshal(body, &active))
	require.Equal(t, pending.Data.ID, active.ID)
	other := th.CreatePublicChannel(t)
	upload, _, err := th.Client.UploadFile(context.Background(), []byte("added scope attachment"), other.Id, "new.bin")
	require.NoError(t, err)
	_, _, err = th.Client.CreatePost(context.Background(), &model.Post{ChannelId: other.Id, Message: "رسالة النطاق المضاف", FileIds: model.StringArray{upload.FileInfos[0].Id}})
	require.NoError(t, err)
	newInfo, storeErr := th.Store.FileInfo().GetFromMaster(upload.FileInfos[0].Id)
	require.NoError(t, storeErr)
	newPreview := newInfo.Path + ".preview"
	_, err = c.fence.db.Exec(`UPDATE fileinfo SET previewpath=$2 WHERE id=$1`, newInfo.Id, newPreview)
	require.NoError(t, err)
	change := caseAPIChange{RequestKey: model.NewId(), Reason: "نطاق مع مرفق معلق", Add: []caseAPIScope{{"channels", other.Id}}, Remove: []caseAPIScope{{"channels", th.BasicChannel.Id}}}
	code, _, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "POST", "/cases/"+active.ID+"/scope", change)
	require.Equal(t, 202, code, string(body))
	var scopePending struct {
		Data caseAPIScopeStatus `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &scopePending))
	code, _, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "GET", "/cases/"+active.ID, nil)
	require.Equal(t, 200, code, string(body))
	require.Contains(t, string(body), scopePending.Data.ID)
	require.Contains(t, string(body), th.BasicChannel.Id)
	_, err = c.source.WriteFile(bytes.NewReader([]byte("new preview restored")), newPreview)
	require.NoError(t, err)
	code, _, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "POST", "/scope_requests/"+scopePending.Data.ID+"/resume", struct{}{})
	require.Equal(t, 200, code, string(body))
	require.Contains(t, string(body), `"state":"complete"`)
}

func TestCaseAPINativeCSRFAndMFAChecks(t *testing.T) {
	th, c, _, _, base := caseAPIFixture(t)
	input := caseAPIStart{model.NewId(), "فحوص المصادقة الأصلية", []caseAPIScope{{"channels", th.BasicChannel.Id}}}
	data, err := json.Marshal(input)
	require.NoError(t, err)
	r, err := http.NewRequest("POST", base+CaseAPIPath+"/cases", bytes.NewReader(data))
	require.NoError(t, err)
	r.AddCookie(&http.Cookie{Name: model.SessionCookieToken, Value: th.SystemAdminClient.AuthToken})
	r.Header.Set("Accept-Language", "ar")
	res, err := http.DefaultClient.Do(r)
	require.NoError(t, err)
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())
	require.Equal(t, 401, res.StatusCode, string(body))
	session, appErr := th.App.GetSession(th.SystemAdminClient.AuthToken)
	require.Nil(t, appErr)
	r, err = http.NewRequest("POST", base+CaseAPIPath+"/cases", bytes.NewReader(data))
	require.NoError(t, err)
	r.AddCookie(&http.Cookie{Name: model.SessionCookieToken, Value: th.SystemAdminClient.AuthToken})
	r.Header.Set(model.HeaderCsrfToken, session.GetCSRF())
	res, err = http.DefaultClient.Do(r)
	require.NoError(t, err)
	body, err = io.ReadAll(res.Body)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())
	require.Equal(t, 200, res.StatusCode, string(body))
	th.Server.SetLicense(model.NewTestLicense("compliance", "mfa"))
	th.App.UpdateConfig(func(cfg *model.Config) {
		*cfg.ServiceSettings.EnableMultifactorAuthentication = true
		*cfg.ServiceSettings.EnforceMultifactorAuthentication = true
	})
	code, _, body := caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "GET", "/cases", nil)
	require.Equal(t, 403, code, string(body))
	var denied int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.api_results WHERE http_status IN(401,403)`).Scan(&denied))
	require.Equal(t, 2, denied)
}

func TestCaseAPIActualRequestCancellationRetainsAuditWithoutDomainCommit(t *testing.T) {
	th, c, _, _, base := caseAPIFixture(t)
	lease, err := c.vault.mutationLease(context.Background())
	require.NoError(t, err)
	defer lease.Close()
	input := caseAPIStart{model.NewId(), "طلب HTTP ملغى", []caseAPIScope{{"channels", th.BasicChannel.Id}}}
	data, err := json.Marshal(input)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, "POST", base+CaseAPIPath+"/cases", bytes.NewReader(data))
	require.NoError(t, err)
	r.Header.Set("Authorization", "Bearer "+th.SystemAdminClient.AuthToken)
	done := make(chan error, 1)
	go func() {
		res, err := http.DefaultClient.Do(r)
		if res != nil {
			_ = res.Body.Close()
		}
		done <- err
	}()
	require.Eventually(t, func() bool {
		var n int
		err := c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.api_actions WHERE request_key=$1`, input.RequestKey).Scan(&n)
		return err == nil && n == 1
	}, 5*time.Second, 10*time.Millisecond, "audit admission must commit before waiting for the archive lease")
	cancel()
	select {
	case err = <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP cancellation did not return")
	}
	require.Eventually(t, func() bool {
		var count int
		err := c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.api_results o JOIN workspace_legal_hold.api_actions a USING(request_id) WHERE a.request_key=$1 AND o.outcome='interrupted'`, input.RequestKey).Scan(&count)
		return err == nil && count == 1
	}, 5*time.Second, 10*time.Millisecond, "detached final audit must survive client cancellation")
	var cases int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.cases`).Scan(&cases))
	require.Zero(t, cases)
	require.NoError(t, lease.Close())
	code, _, body := caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "POST", "/cases", input)
	require.Equal(t, 200, code, string(body))
}

func TestCaseAPIDownloadRangesIntegrityAndAuditAdmissionFailure(t *testing.T) {
	th, c, m, a, base := caseAPIFixture(t)
	_, _ = uploadFixtureFile(t, th, bytes.Repeat([]byte("download byte evidence\x00"), 16000))
	rctx := th.Context.WithSession(&model.Session{UserId: th.SystemAdminUser.Id})
	status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
	exported, err := a.exporter.ExportWithKey(rctx, status.ID, model.NewId())
	require.NoError(t, err)
	file, _, err := a.exporter.Open(rctx, exported.ID)
	require.NoError(t, err)
	expected := make([]byte, 100)
	_, err = file.ReadAt(expected, 10)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	r, err := http.NewRequest("GET", base+CaseAPIPath+"/exports/"+exported.ID+"/download", nil)
	require.NoError(t, err)
	r.Header.Set("Authorization", "Bearer "+th.SystemAdminClient.AuthToken)
	r.Header.Set("Range", "bytes=10-109")
	res, err := http.DefaultClient.Do(r)
	require.NoError(t, err)
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())
	require.Equal(t, 206, res.StatusCode)
	require.Equal(t, expected, body)
	var written int64
	require.NoError(t, c.fence.db.QueryRow(`SELECT bytes_written FROM workspace_legal_hold.api_results WHERE request_id=$1`, res.Header.Get("X-Legal-Hold-Request-Id")).Scan(&written))
	require.Equal(t, int64(100), written)
	_, err = c.fence.db.Exec(`CREATE FUNCTION public.reject_legal_download_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'private download rejection'; END $$;
 CREATE TRIGGER fixture_reject BEFORE INSERT ON workspace_legal_hold.api_downloads FOR EACH ROW EXECUTE FUNCTION public.reject_legal_download_fixture()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := c.fence.db.Exec(`DROP FUNCTION IF EXISTS public.reject_legal_download_fixture() CASCADE`)
		require.NoError(t, err)
	})
	code, _, body := caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "GET", "/exports/"+exported.ID+"/download", nil)
	require.Equal(t, 503, code, string(body))
	require.NotContains(t, string(body), "private download rejection")
	require.False(t, bytes.HasPrefix(body, []byte("PK")))
	_, err = c.fence.db.Exec(`DROP TRIGGER fixture_reject ON workspace_legal_hold.api_downloads`)
	require.NoError(t, err)
	// Published artifacts are read-only. Deliberately make this fixture
	// writable to simulate out-of-band corruption as an unprivileged user.
	name := "packages/" + exported.ID + ".zip"
	info, err := a.exporter.root.Stat(name)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o400), info.Mode().Perm())
	require.NoError(t, a.exporter.root.Chmod(name, 0o600))
	corrupt, err := a.exporter.root.OpenFile(name, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = corrupt.WriteAt([]byte("corruption"), 0)
	require.NoError(t, err)
	require.NoError(t, corrupt.Close())
	code, _, body = caseAPIHTTP(t, base, th.SystemAdminClient.AuthToken, "GET", "/exports/"+exported.ID+"/download", nil)
	require.Equal(t, 409, code, string(body))
	require.False(t, bytes.HasPrefix(body, []byte("PK")))
	require.Contains(t, string(body), "enterprise.legal_hold.integrity")
}

type caseAPIInterruptedWriter struct {
	*httptest.ResponseRecorder
	first   func()
	written int
	fail    bool
}

func (w *caseAPIInterruptedWriter) Write(p []byte) (int, error) {
	if w.fail && w.written > 0 {
		return 0, errors.New("fixture client stopped reading")
	}
	n, err := w.ResponseRecorder.Write(p)
	w.written += n
	if w.first != nil {
		first := w.first
		w.first = nil
		first()
	}
	return n, err
}

func TestCaseAPIDownloadInterruptedOrRevokedDoesNotClaimDelivery(t *testing.T) {
	for _, fault := range []string{"client-write-error", "license-revoked", "cancelled"} {
		t.Run(fault, func(t *testing.T) {
			th, c, m, a, _ := caseAPIFixture(t)
			_, _ = uploadFixtureFile(t, th, bytes.Repeat([]byte("stream fixture without compression\x00"), 14000))
			rctx := th.Context.WithSession(&model.Session{UserId: th.SystemAdminUser.Id})
			status := startChannelCase(t, m, rctx, th.BasicChannel.Id)
			exported, err := a.exporter.ExportWithKey(rctx, status.ID, model.NewId())
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := httptest.NewRequest("GET", CaseAPIPath+"/exports/"+exported.ID+"/download", nil).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+th.SystemAdminClient.AuthToken)
			w := &caseAPIInterruptedWriter{ResponseRecorder: httptest.NewRecorder(), fail: fault == "client-write-error"}
			w.first = func() {
				var started bool
				require.NoError(t, c.fence.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM workspace_legal_hold.api_downloads WHERE request_id=$1)`, w.Header().Get("X-Legal-Hold-Request-Id")).Scan(&started))
				require.True(t, started, "durable download admission must precede even the first byte")
				if fault == "license-revoked" {
					th.Server.SetLicense(nil)
				} else if fault == "cancelled" {
					cancel()
				}
			}
			a.ServeHTTP(w, r)
			require.Greater(t, w.written, 0)
			require.Less(t, int64(w.written), exported.Size)
			var outcome string
			var count int64
			var statusCode int
			require.NoError(t, c.fence.db.QueryRow(`SELECT outcome,bytes_written,http_status FROM workspace_legal_hold.api_results WHERE request_id=$1`, w.Header().Get("X-Legal-Hold-Request-Id")).Scan(&outcome, &count, &statusCode))
			require.Equal(t, "interrupted", outcome)
			require.Equal(t, int64(w.written), count)
			require.Equal(t, 200, statusCode, "headers were already sent; a later authorization failure cannot change their status")
			require.NotContains(t, w.Body.String(), "enterprise.legal_hold.", "a partial ZIP must not acquire a JSON error suffix")
		})
	}
}

func TestCaseAPIAuditSchemaGuardsAndExplicitConstruction(t *testing.T) {
	_, c, m, _, _ := caseFixture(t)
	e, _ := exportFixture(t, m, 8<<20)
	_, err := NewCaseAPI(m, e)
	require.ErrorContains(t, err, "API audit")
	before := 0
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_versions`).Scan(&before))
	require.NoError(t, InstallCaseAPIAudit(context.Background(), m.journal))
	require.NoError(t, InstallCaseAPIAudit(context.Background(), m.journal))
	var after int
	require.NoError(t, c.fence.db.QueryRow(`SELECT count(*) FROM workspace_legal_hold.case_versions`).Scan(&after))
	require.Equal(t, before, after)
	for _, table := range []string{"api_audit_installation", "api_requests", "api_identities", "api_actions", "api_downloads", "api_results"} {
		_, err = c.fence.db.Exec("TRUNCATE workspace_legal_hold." + table + " CASCADE")
		require.Error(t, err, table)
	}
	_, err = c.fence.db.Exec(`ALTER TABLE workspace_legal_hold.api_actions DISABLE TRIGGER require_authorized`)
	require.NoError(t, err)
	_, err = NewCaseAPI(m, e)
	require.ErrorContains(t, err, "triggers are incomplete")
	_, err = c.fence.db.Exec(`ALTER TABLE workspace_legal_hold.api_actions ENABLE ALWAYS TRIGGER require_authorized`)
	require.NoError(t, err)
	_, err = NewCaseAPI(m, e)
	require.NoError(t, err)
	_, err = c.fence.db.Exec(`INSERT INTO workspace_legal_hold.api_requests(id,operation,method) VALUES($1,'get_case','GET')`, model.NewId())
	require.NoError(t, err)
	_, err = c.fence.db.Exec(`DELETE FROM workspace_legal_hold.api_requests`)
	require.ErrorContains(t, err, "append only")
	_, err = c.fence.db.Exec(`UPDATE workspace_legal_hold.api_requests SET operation='forged'`)
	require.ErrorContains(t, err, "append only")
	_, err = c.fence.db.Exec(`SET session_replication_role='replica'; DELETE FROM workspace_legal_hold.api_requests`)
	require.ErrorContains(t, err, "append only")
}

var _ = strings.Builder{}
