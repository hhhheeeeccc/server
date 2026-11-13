package brand

import (
	_ "embed"
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

//go:embed default_brand_image.png
var defaultImage []byte

type Service struct {
	app *app.App
}

func Register(a *app.App) {
	service := &Service{app: a}
	handler := service.getImage
	a.Srv().Router.Handle(model.APIURLSuffix+"/brand/image", &web.Handler{
		Srv:            a.Srv(),
		HandleFunc:     handler,
		HandlerName:    web.GetHandlerName(handler),
		RequireSession: false,
		TrustRequester: true,
	}).Methods(http.MethodGet)
}

func (s *Service) getImage(c *web.Context, w http.ResponseWriter, _ *http.Request) {
	if s.app.License() == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	image, appErr := c.App.GetBrandImage(c.AppContext)
	if appErr != nil {
		image = defaultImage
	}

	w.Header().Set("Content-Type", "image/png")
	if _, err := w.Write(image); err != nil {
		c.Logger.Warn("Error while writing response", mlog.Err(err))
	}
}
