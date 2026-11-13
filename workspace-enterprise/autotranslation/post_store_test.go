package autotranslation

import (
	"errors"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/store"
	"github.com/stretchr/testify/require"
)

type committedPostFixture struct {
	store.PostStore
	stored         *model.Post
	readErr        error
	writeErr       error
	reads          int
	primary        bool
	includeDeleted bool
}

func (s *committedPostFixture) Update(_ request.CTX, post, _ *model.Post) (*model.Post, error) {
	return post, s.writeErr
}

func (s *committedPostFixture) Overwrite(_ request.CTX, post *model.Post) (*model.Post, error) {
	return post, s.writeErr
}

func (s *committedPostFixture) OverwriteMultiple(_ request.CTX, posts []*model.Post) ([]*model.Post, int, error) {
	return posts, -1, s.writeErr
}

func (s *committedPostFixture) GetSingle(rctx request.CTX, _ string, includeDeleted bool) (*model.Post, error) {
	s.reads++
	s.primary = store.HasMaster(rctx.Context())
	s.includeDeleted = includeDeleted
	return s.stored, s.readErr
}

func TestCommittedPostWritesUsePrimarySnapshot(t *testing.T) {
	for _, operation := range []string{"update", "overwrite", "multiple"} {
		t.Run(operation, func(t *testing.T) {
			candidate := &model.Post{Id: model.NewId(), Message: "source", UpdateAt: 100,
				Metadata: &model.PostMetadata{Translations: map[string]*model.PostTranslation{"ar": {Text: "same-source metadata"}}}}
			stored := candidate.Clone()
			stored.UpdateAt = 101
			stored.Metadata = nil
			stored.ReplyCount = 7
			fixture := &committedPostFixture{stored: stored}
			s := &committedPostStore{PostStore: fixture}
			var result *model.Post
			var err error
			rctx := request.EmptyContext(nil)
			switch operation {
			case "update":
				result, err = s.Update(rctx, candidate, candidate.Clone())
			case "overwrite":
				result, err = s.Overwrite(rctx, candidate)
			case "multiple":
				var posts []*model.Post
				posts, _, err = s.OverwriteMultiple(rctx, []*model.Post{candidate})
				require.Len(t, posts, 1)
				result = posts[0]
			}
			require.NoError(t, err)
			require.EqualValues(t, 101, result.UpdateAt)
			require.Same(t, candidate.Metadata, result.Metadata, "preserve incoming metadata for the same persisted payload")
			require.True(t, fixture.primary, "read-back must never use a lagging replica")
			require.True(t, fixture.includeDeleted, "a concurrent soft deletion must replace the live snapshot")
		})
	}
}

func TestCommittedPostSupersededPayloadDoesNotInheritMetadata(t *testing.T) {
	candidate := &model.Post{Id: model.NewId(), Message: "old source", UpdateAt: 100,
		Metadata: &model.PostMetadata{Translations: map[string]*model.PostTranslation{"ar": {Text: "old private metadata"}}}}
	for _, change := range []string{"message", "files", "props", "deleted"} {
		t.Run(change, func(t *testing.T) {
			stored := candidate.Clone()
			stored.UpdateAt, stored.Metadata = 101, nil
			switch change {
			case "message":
				stored.Message = "new source"
			case "files":
				stored.FileIds = model.StringArray{model.NewId()}
			case "props":
				stored.AddProp("new-prop", "new payload")
			case "deleted":
				stored.DeleteAt = 101
			}
			s := &committedPostStore{PostStore: &committedPostFixture{stored: stored}}
			result, err := s.Update(request.EmptyContext(nil), candidate, candidate.Clone())
			require.NoError(t, err)
			require.Same(t, stored, result, "return the whole winning snapshot, never its timestamp on the old source")
			require.Nil(t, result.Metadata)
		})
	}
}

func TestCommittedPostReadFailureDoesNotReturnUnorderedCandidate(t *testing.T) {
	for _, operation := range []string{"update", "overwrite", "multiple"} {
		t.Run(operation, func(t *testing.T) {
			fixture := &committedPostFixture{readErr: errors.New("primary unavailable")}
			s := &committedPostStore{PostStore: fixture}
			post := &model.Post{Id: model.NewId()}
			rctx := request.EmptyContext(nil)
			var err error
			switch operation {
			case "update":
				_, err = s.Update(rctx, post, post.Clone())
			case "overwrite":
				_, err = s.Overwrite(rctx, post)
			case "multiple":
				_, _, err = s.OverwriteMultiple(rctx, []*model.Post{post})
			}
			require.ErrorIs(t, err, fixture.readErr)
			require.Equal(t, 1, fixture.reads)
			fixture.writeErr = errors.New("write rejected")
			_, err = s.Update(rctx, post, post.Clone())
			require.ErrorIs(t, err, fixture.writeErr)
			require.Equal(t, 1, fixture.reads, "a failed write must not read or return another snapshot")
		})
	}
}
