package content_extraction_test

import (
	"encoding/json"
	"fmt"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
	"github.com/mattermost/mattermost/server/v8/config"
	"os"
	"time"
)

const extractionProcessEnv = "WORKSPACE_TEST_EXTRACTION_PROCESS"

type extractionProcessInput struct {
	Config            *model.Config
	JobID, ResultPath string
}

type extractionProcessResult struct {
	PID    int
	Status string
}

// The test executable's child entry runs a real server. Central Enterprise
// registration and the native watcher install and dispatch the worker.
func runExtractionProcess(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var input extractionProcessInput
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
	mem, err := config.NewMemoryStoreWithOptions(&config.MemoryStoreOptions{IgnoreEnvironmentOverrides: true})
	if err != nil {
		return err
	}
	if err := mem.Set(input.Config); err != nil {
		return err
	}
	configuration, err := config.NewStoreFromBacking(mem, nil, false)
	if err != nil {
		return err
	}
	defer configuration.Close()
	jobs.DefaultWatcherPollingInterval = 20
	server, err := app.NewServer(app.ConfigStore(configuration), app.SetLogger(logger), app.DisableGlobalLogger())
	if err != nil {
		return err
	}
	defer server.Shutdown()
	if !server.SetLicense(model.NewTestLicenseSKU(model.LicenseShortSkuEnterpriseAdvanced)) {
		return fmt.Errorf("child rejected explicit extraction test license")
	}
	if err := server.Start(); err != nil {
		return err
	}
	if err := server.Jobs.StartWorkers(); err != nil {
		return err
	}
	defer server.Jobs.StopWorkers()
	deadline := time.Now().Add(20 * time.Second)
	for {
		job, err := server.Store().Job().Get(request.EmptyContext(logger), input.JobID)
		if err != nil {
			return err
		}
		if job.Status == model.JobStatusSuccess || job.Status == model.JobStatusError || job.Status == model.JobStatusCanceled {
			data, err := json.Marshal(extractionProcessResult{PID: os.Getpid(), Status: job.Status})
			if err != nil {
				return err
			}
			return os.WriteFile(input.ResultPath, data, 0600)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("watcher did not complete extraction Job: status=%s", job.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
