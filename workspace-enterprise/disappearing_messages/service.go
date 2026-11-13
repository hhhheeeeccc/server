package disappearing_messages

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/store"
)

type Service struct {
	app  *app.App
	repo repository
}

var services sync.Map

// Register uses only the public Enterprise registration boundary. Native post
// creation, permissions, storage, deletion and WebSocket handlers remain owners.
func Register(a *app.App) {
	s := &Service{app: a, repo: repository{a.Srv().Store().GetInternalMasterDB()}}
	if err := s.repo.install(a.Srv().Platform().GoContext()); err != nil {
		// Do not expose a setting that would send untracked messages.
		a.Log().Error("Unable to install disappearing-message policy", mlog.Err(err))
		return
	}
	if _, loaded := services.LoadOrStore(a.Srv(), s); loaded {
		return
	}
	s.registerAPI()
	a.Srv().Router.Use(s.protectFiles)
	a.Srv().Go(func() {
		defer services.Delete(a.Srv())
		s.run(a.Srv().Platform().GoContext())
	})
}

func (s *Service) available() bool { return s.app.License() != nil }

func (s *Service) run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := s.expireBatch(ctx); err != nil && ctx.Err() == nil {
			s.app.Log().Warn("Unable to expire conversation messages", mlog.Err(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) expireBatch(ctx context.Context) error {
	// Bounded, indexed work. No timer/goroutine per message and no full-history
	// scans. Existing deadlines continue to be honored if a license expires.
	for i := 0; i < 25 && ctx.Err() == nil; i++ {
		m, err := s.repo.claim(ctx)
		if err != nil || m == nil {
			return err
		}
		if err := s.expireOne(ctx, m); err != nil {
			s.app.Log().Warn("Conversation message expiry will be retried", mlog.String("post_id", m.PostID), mlog.Err(err))
			continue
		}
		if err := s.repo.complete(ctx, m); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *Service) expireOne(ctx context.Context, m *dueMessage) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rctx := request.EmptyContext(s.app.Log()).WithContext(ctx)
	post, err := s.app.Srv().Store().Post().GetSingle(store.RequestContextWithMaster(rctx), m.PostID, true)
	if err != nil {
		var notFound *store.ErrNotFound
		if errors.As(err, &notFound) {
			return nil // already permanently removed by native retention
		}
		return err
	}
	rctx = rctx.WithSession(&model.Session{UserId: post.UserId})
	if post.DeleteAt == 0 {
		if _, appErr := s.app.DeletePost(rctx, post.Id, post.UserId); appErr != nil {
			return appErr
		}
	} else if appErr := s.app.CleanUpAfterPostDeletion(rctx, post, post.UserId); appErr != nil {
		// Crash recovery after a committed delete must still deliver its event
		// and invalidate caches without decrementing native threads twice.
		return appErr
	}
	// DeletePost schedules this step asynchronously. Confirm it synchronously
	// before acknowledging the durable queue, including crash/manual deletion.
	if _, err := s.app.Srv().Store().FileInfo().DeleteForPost(rctx, post.Id); err != nil {
		return err
	}
	s.app.Srv().Store().FileInfo().InvalidateFileInfosForPostCache(post.Id, true)
	s.app.Srv().Store().FileInfo().InvalidateFileInfosForPostCache(post.Id, false)
	return nil
}
