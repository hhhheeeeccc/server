package message_export

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
	"github.com/mattermost/mattermost/server/v8/config"
)

const messageExportProcessEnv = "WORKSPACE_MESSAGE_EXPORT_TEST_PROCESS"

type messageExportProcessInput struct {
	Config     *model.Config
	JobID      string
	ReadyPath  string
	StartPath  string
	ResultPath string
}

type messageExportProcessResult struct {
	PID    int
	Status string
	Error  string
}

func runMessageExportProcess(path string) int {
	err := func() error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var input messageExportProcessInput
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
		jobs.DefaultWatcherPollingInterval = 20
		server, err := app.NewServer(app.ConfigStore(configuration), app.SetLogger(logger), app.DisableGlobalLogger())
		if err != nil {
			return err
		}
		defer server.Shutdown()
		if !server.SetLicense(model.NewTestLicense("message_export")) {
			return fmt.Errorf("child rejected explicit export test license")
		}
		if err := server.Start(); err != nil {
			return err
		}
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
		if err := server.Jobs.StartWorkers(); err != nil {
			return err
		}
		defer server.Jobs.StopWorkers()
		result := messageExportProcessResult{PID: os.Getpid()}
		deadline = time.Now().Add(8 * time.Second)
		for {
			job, err := server.Store().Job().Get(request.EmptyContext(logger), input.JobID)
			if err != nil {
				return err
			}
			result.Status = job.Status
			if job.Status == model.JobStatusSuccess {
				break
			}
			if time.Now().After(deadline) {
				result.Error = "actual message-export watcher did not recover the crashed claim: " + job.Status
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		data, err = json.Marshal(result)
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
