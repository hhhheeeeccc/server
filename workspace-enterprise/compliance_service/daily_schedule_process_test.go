package compliance_service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
	"github.com/mattermost/mattermost/server/v8/config"
)

const dailyScheduleProcessEnv = "WORKSPACE_TEST_COMPLIANCE_SCHEDULE_PROCESS"

type dailyScheduleProcessInput struct {
	Config     *model.Config
	EndAt      int64
	ResultPath string
	Mode       string
	ReadyPath  string
	StartPath  string
	ReportID   string
}

type dailyScheduleProcessResult struct {
	PID     int
	Through int64
	Status  string
	Count   int
}

// TestMain dispatches here before constructing its own fixture database. Each
// child opens the parent's isolated PostgreSQL using a real App and SQL store.
func runDailyScheduleProcess(inputPath string) int {
	err := func() error {
		data, err := os.ReadFile(inputPath)
		if err != nil {
			return err
		}
		var input dailyScheduleProcessInput
		if err := json.Unmarshal(data, &input); err != nil {
			return err
		}
		if err := utils.TranslationsPreInit(); err != nil {
			return err
		}
		logger, err := mlog.NewLogger()
		if err != nil {
			return err
		}
		defer logger.Shutdown()
		memory, err := config.NewMemoryStoreWithOptions(&config.MemoryStoreOptions{IgnoreEnvironmentOverrides: true})
		if err != nil {
			return err
		}
		if err := memory.Set(input.Config); err != nil {
			return err
		}
		configuration, err := config.NewStoreFromBacking(memory, nil, false)
		if err != nil {
			return err
		}
		defer configuration.Close()
		server, err := app.NewServer(app.ConfigStore(configuration), app.SetLogger(logger), app.DisableGlobalLogger())
		if err != nil {
			return err
		}
		defer server.Shutdown()
		license := model.NewTestLicense()
		license.Features.Compliance = model.NewPointer(true)
		if !server.SetLicense(license) {
			return fmt.Errorf("child rejected explicit compliance test license")
		}
		service := &Service{app: app.New(app.ServerConnector(server.Channels()))}
		if input.Mode == "export" || input.Mode == "adhoc-export" || input.Mode == "adhoc-recover" {
			if err := os.WriteFile(input.ReadyPath, []byte("ready"), 0o600); err != nil {
				return err
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(input.StartPath); err == nil {
					break
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("export child start signal timed out")
				}
				time.Sleep(10 * time.Millisecond)
			}
			result := dailyScheduleProcessResult{PID: os.Getpid()}
			switch input.Mode {
			case "export":
				if err := service.runDailyInterval(context.Background(), time.UnixMilli(input.EndAt).UTC()); err != nil {
					return err
				}
				if err := server.Store().GetInternalMasterDB().QueryRow(`SELECT Status,Count FROM Compliances WHERE Type=$1 AND EndAt=$2`,
					model.ComplianceTypeDaily, input.EndAt).Scan(&result.Status, &result.Count); err != nil {
					return err
				}
			case "adhoc-export":
				if err := service.RunComplianceJob(request.EmptyContext(logger), &model.Compliance{Id: input.ReportID}); err != nil {
					return err
				}
			case "adhoc-recover":
				service.StartComplianceDailyJob()
			}
			if input.Mode != "export" {
				deadline := time.Now().Add(10 * time.Second)
				for {
					if err := server.Store().GetInternalMasterDB().QueryRow(`SELECT Status,Count FROM Compliances WHERE Id=$1`,
						input.ReportID).Scan(&result.Status, &result.Count); err != nil {
						return err
					}
					if result.Status == model.ComplianceStatusFinished {
						break
					}
					if time.Now().After(deadline) {
						return fmt.Errorf("adhoc child did not finish the persisted report: %s", result.Status)
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			data, err := json.Marshal(result)
			if err != nil {
				return err
			}
			return os.WriteFile(input.ResultPath, data, 0o600)
		}
		if err := service.prepareDailyIntervals(context.Background(), time.UnixMilli(input.EndAt).UTC()); err != nil {
			return err
		}
		var raw string
		if err := server.Store().GetInternalMasterDB().QueryRow(`SELECT Value FROM Systems WHERE Name=$1`, dailyScheduleKey).Scan(&raw); err != nil {
			return err
		}
		through, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return err
		}
		data, err = json.Marshal(dailyScheduleProcessResult{PID: os.Getpid(), Through: through})
		if err != nil {
			return err
		}
		return os.WriteFile(input.ResultPath, data, 0o600)
	}()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
