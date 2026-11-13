package account_migration

import (
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/store"
)

type migrationStore struct {
	store.Store
}

func (s *migrationStore) Session() store.SessionStore {
	return primarySessionLookup{SessionStore: s.Store.Session()}
}

type primarySessionLookup struct {
	store.SessionStore
}

func (s primarySessionLookup) Get(rctx request.CTX, idOrToken string) (*model.Session, error) {
	// A replica can retain a deleted session after migration clears its cache.
	// Rehydrating that row would authenticate and cache a revoked credential.
	// Keep the native lookup and cache, selecting its existing primary context.
	return s.SessionStore.Get(store.RequestContextWithMaster(rctx), idOrToken)
}

func installPrimarySessionLookups(a *app.App) {
	RegisterPrimarySessionLookupsBeforeStart(a.Srv())
}

// RegisterPrimarySessionLookupsBeforeStart installs the adapter before platform
// hubs and user services capture the store. main.go calls it from its server
// Enterprise constructor. Later AccountMigration.New calls are idempotent even
// when another Enterprise adapter wraps this store.
func RegisterPrimarySessionLookupsBeforeStart(server *app.Server) {
	current := server.Store()
	if _, installed := current.Session().(primarySessionLookup); !installed {
		server.SetStore(&migrationStore{Store: current})
	}
}
