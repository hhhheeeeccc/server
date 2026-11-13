package nova_sms

import (
	"context"
	"database/sql"

	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
)

type Service struct {
	app     *app.App
	db      *sql.DB
	initErr error
}

func Register(a *app.App) *Service {
	s := &Service{app: a, db: a.Srv().Store().GetInternalMasterDB()}
	s.initErr = s.initSettings(context.Background())
	if s.initErr != nil {
		a.Log().Error("Unable to initialize Nova connection settings", mlog.Err(s.initErr))
	} else if err := s.initDelivery(context.Background()); err != nil {
		s.initErr = err
		a.Log().Error("Unable to initialize Nova delivery queue", mlog.Err(err))
	} else {
		a.Srv().Go(s.runDelivery)
	}
	s.registerRoutes()
	return s
}
