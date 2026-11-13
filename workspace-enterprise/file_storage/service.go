package file_storage

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"syscall"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

type Usage struct {
	DriverName      string `json:"driver_name"`
	HasCapacityInfo bool   `json:"has_capacity_info"`
	TotalBytes      int64  `json:"total_bytes"`
	FreeBytes       int64  `json:"free_bytes"`
	UsedBytes       int64  `json:"used_bytes"`
}

type Service struct {
	app                 *app.App
	downloadRecordSlots chan struct{}
}

func Register(a *app.App) {
	service := &Service{app: a, downloadRecordSlots: make(chan struct{}, maxConcurrentDownloadRecords)}
	if err := service.initializeDownloadStatistics(); err != nil {
		a.Log().Error("Unable to initialize file download statistics", mlog.Err(err))
	}
	a.Srv().Router.Use(service.analyticsAndDownloadMiddleware)

	handler := service.getUsage

	a.Srv().Router.Handle(model.APIURLSuffix+"/file_storage/usage", &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: true,
		RequireMfa:     true,
	}).Methods(http.MethodGet)
}

func (s *Service) getUsage(c *web.Context, w http.ResponseWriter, _ *http.Request) {
	if s.app.License() == nil {
		c.Err = model.NewAppError("GetFileStorageUsage", "api.license.upgrade_needed.app_error", nil, "", http.StatusForbidden)
		return
	}
	if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionSysconsoleReadEnvironmentFileStorage) {
		c.SetPermissionError(model.PermissionSysconsoleReadEnvironmentFileStorage)
		return
	}

	usage, appErr := s.usage(c.AppContext.Context())
	if appErr != nil {
		c.Err = appErr
		return
	}
	if err := json.NewEncoder(w).Encode(usage); err != nil {
		c.Logger.Warn("Error while writing response", mlog.Err(err))
	}
}

func (s *Service) usage(ctx context.Context) (*Usage, *model.AppError) {
	backend := s.app.FileBackend()
	usage := &Usage{DriverName: backend.DriverName()}

	if usage.DriverName == model.ImageDriverLocal {
		directory := model.SafeDereference(s.app.Config().FileSettings.Directory)
		if directory == "" {
			directory = "."
		}
		if _, err := os.Stat(directory); err != nil {
			return nil, storageError(err)
		}

		var stats syscall.Statfs_t
		if err := syscall.Statfs(directory, &stats); err != nil {
			return nil, storageError(err)
		}

		usage.HasCapacityInfo = true
		usage.TotalBytes = int64(stats.Blocks) * int64(stats.Bsize)
		usage.FreeBytes = int64(stats.Bavail) * int64(stats.Bsize)
		usage.UsedBytes = usage.TotalBytes - usage.FreeBytes
		return usage, nil
	}

	// The core GetStorageUsage value comes from the file_stats materialized
	// view, which can remain at zero until its scheduled refresh after uploads.
	var used int64
	if err := s.app.Srv().Store().GetInternalMasterDB().QueryRowContext(ctx,
		`SELECT COALESCE(SUM(Size), 0) FROM FileInfo WHERE DeleteAt = 0`).Scan(&used); err != nil {
		return nil, storageError(err)
	}
	usage.UsedBytes = used
	return usage, nil
}

func storageError(err error) *model.AppError {
	return model.NewAppError("GetFileStorageUsage", "api.file.storage_usage.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
}
