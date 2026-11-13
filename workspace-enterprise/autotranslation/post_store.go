package autotranslation

import (
	"fmt"
	"reflect"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/store"
)

type postWriteStore struct {
	store.Store
	posts *committedPostStore
}

func (s *postWriteStore) Post() store.PostStore { return s.posts }

// RegisterPostWrites retains the App entry point for idempotent callers. Initial
// installation belongs in RegisterPostWritesBeforeStart, before platform hubs.
func RegisterPostWrites(a *app.App) {
	RegisterPostWritesBeforeStart(a.Srv())
}

// RegisterPostWritesBeforeStart pairs source ordering with native post objects.
// Install from the server Enterprise constructor, before Platform.Start and
// before services capture the store. InstallPostLifecycle must finish before
// APIs are served. Like deletion hygiene, ordering does not unlock translation.
func RegisterPostWritesBeforeStart(server *app.Server) {
	current := server.Store()
	if _, installed := current.Post().(*committedPostStore); installed {
		return
	}
	server.SetStore(&postWriteStore{Store: current, posts: &committedPostStore{PostStore: current.Post()}})
}

type committedPostStore struct {
	store.PostStore
}

func (s *committedPostStore) Update(rctx request.CTX, newPost, oldPost *model.Post) (*model.Post, error) {
	post, err := s.PostStore.Update(rctx, newPost, oldPost)
	if err != nil {
		return nil, err
	}
	return s.committed(rctx, post)
}

func (s *committedPostStore) Overwrite(rctx request.CTX, post *model.Post) (*model.Post, error) {
	updated, err := s.PostStore.Overwrite(rctx, post)
	if err != nil {
		return nil, err
	}
	return s.committed(rctx, updated)
}

func (s *committedPostStore) OverwriteMultiple(rctx request.CTX, posts []*model.Post) ([]*model.Post, int, error) {
	updated, index, err := s.PostStore.OverwriteMultiple(rctx, posts)
	if err != nil {
		return nil, index, err
	}
	for i, post := range updated {
		updated[i], err = s.committed(rctx, post)
		if err != nil {
			return nil, i, err
		}
	}
	return updated, index, nil
}

func (s *committedPostStore) committed(rctx request.CTX, candidate *model.Post) (*model.Post, error) {
	// SqlPostStore returns the input object, not UPDATE ... RETURNING. Reload
	// from the primary to carry the trigger's committed timestamp. If another
	// edit already won, return that complete snapshot; attaching its timestamp
	// to this candidate's old source would defeat the reducer's ordering guard.
	stored, err := s.PostStore.GetSingle(store.RequestContextWithMaster(rctx), candidate.Id, true)
	if err != nil {
		return nil, fmt.Errorf("read committed post after write: %w", err)
	}
	if samePostPayload(candidate, stored) {
		result := candidate.Clone()
		result.UpdateAt = stored.UpdateAt
		return result, nil
	}
	// A superseding edit/deletion must not inherit this attempt's transient
	// metadata. The native app prepares the returned snapshot for its reader.
	return stored, nil
}

func samePostPayload(a, b *model.Post) bool {
	// Ignore transient fields and the trigger-adjusted clock. Compare all
	// persisted fields before retaining the candidate's incoming metadata.
	normalize := func(post *model.Post) *model.Post {
		copy := post.Clone()
		copy.UpdateAt = 0
		copy.MessageSource = ""
		copy.PendingPostId = ""
		copy.ReplyCount = 0
		copy.LastReplyAt = 0
		copy.Participants = nil
		copy.IsFollowing = nil
		copy.Metadata = nil
		if len(copy.FileIds) == 0 {
			copy.FileIds = nil
		}
		if len(copy.Filenames) == 0 {
			copy.Filenames = nil
		}
		return copy
	}
	return reflect.DeepEqual(normalize(a), normalize(b))
}
