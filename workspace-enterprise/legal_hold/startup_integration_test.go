package legal_hold

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mattermost/enterprise/internal/objectstoretest"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/api4"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/testlib"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	"github.com/mattermost/mattermost/server/v8/platform/shared/filestore"
	"github.com/stretchr/testify/require"
)

var startupGuards sync.Map
var startupInitializers sync.Map

type startupFixture struct {
	guard   *fixturePreservationGuard
	wrapped filestore.FileBackend
}

// Register only for directories selected by an individual test. No root
// Enterprise init, global feature enablement, or production route is involved.
func TestMain(m *testing.M) {
	if os.Getenv("WORKSPACE_LEGAL_HOLD_CHILD") == "1" {
		os.Exit(coordinatorChildMain())
	}
	app.RegisterAutoTranslationInterface(func(server *app.Server) einterfaces.AutoTranslationInterface {
		if initialize, ok := startupInitializers.Load(filepath.Clean(*server.Config().FileSettings.Directory)); ok {
			if err := initialize.(func(*app.Server) error)(server); err != nil {
				panic(err)
			}
		}
		if value, ok := startupGuards.Load(filepath.Clean(*server.Config().FileSettings.Directory)); ok {
			fixture := value.(*startupFixture)
			if err := InstallBackendBeforeStart(server, fixture.guard); err != nil {
				panic(err)
			}
			fixture.wrapped = server.FileBackend()
		}
		return nil
	})
	helper := testlib.NewMainHelper()
	defer helper.Close()
	api4.SetMainHelper(helper)
	helper.Main(m)
}

func startupFixtureServer(t *testing.T) (*api4.TestHelper, *fixturePreservationGuard, string) {
	return startupFixtureServerSettings(t, nil)
}

func startupFixtureServerSettings(t *testing.T, settings *model.FileSettings) (*api4.TestHelper, *fixturePreservationGuard, string) {
	t.Helper()
	live := t.TempDir()
	archive := t.TempDir()
	require.NoError(t, os.Chmod(archive, 0o700))
	vault, err := OpenLocalVault(archive, live)
	require.NoError(t, err)
	t.Cleanup(func() { _ = vault.Close() })
	fixture := &startupFixture{guard: newFixtureGuard(vault)}
	key := filepath.Clean(live)
	startupGuards.Store(key, fixture)
	t.Cleanup(func() { startupGuards.Delete(key) })
	th := api4.SetupConfig(t, func(cfg *model.Config) {
		cfg.CacheSettings.CacheType = new(model.CacheTypeLRU)
		if settings != nil {
			cfg.FileSettings = *settings
			cfg.ServiceSettings.AllowedUntrustedInternalConnections = model.NewPointer("127.0.0.1")
		}
		cfg.FileSettings.Directory = &live
		cfg.FileSettings.ExtractContent = model.NewPointer(false)
		cfg.EmailSettings.SendEmailNotifications = model.NewPointer(false)
		cfg.EmailSettings.SendPushNotifications = model.NewPointer(false)
	}).InitBasic(t)
	require.True(t, th.Server.SetLicense(model.NewTestLicense("compliance")))
	require.NotNil(t, fixture.wrapped)
	identity := func(backend filestore.FileBackend) *guardedBackend {
		switch value := backend.(type) {
		case *guardedBackend:
			return value
		case guardedLinkBackend:
			return value.guardedBackend
		default:
			t.Fatalf("backend is not guarded: %T", backend)
			return nil
		}
	}
	require.Same(t, identity(fixture.wrapped), identity(th.Server.FileBackend()))
	require.Same(t, identity(fixture.wrapped), identity(th.App.FileBackend()), "Channels must capture the guarded backend during startup")
	if !model.SafeDereference(th.App.Config().FileSettings.DedicatedExportStore) {
		require.Same(t, identity(fixture.wrapped), identity(th.App.ExportFileBackend()), "shared export alias must not bypass preservation")
	}
	return th, fixture.guard, archive
}

func uploadFixtureFile(t *testing.T, th *api4.TestHelper, data []byte) (*model.Post, *model.FileInfo) {
	t.Helper()
	response, _, err := th.Client.UploadFile(context.Background(), data, th.BasicChannel.Id, "دليل.bin")
	require.NoError(t, err)
	post, _, err := th.Client.CreatePost(context.Background(), &model.Post{ChannelId: th.BasicChannel.Id,
		Message: "رسالة للحفظ", FileIds: model.StringArray{response.FileInfos[0].Id}})
	require.NoError(t, err)
	info, err := th.Store.FileInfo().GetFromMaster(response.FileInfos[0].Id)
	require.NoError(t, err)
	return post, info
}

func TestStartupHookPreservesBytesBeforeNativePermanentAttachmentDelete(t *testing.T) {
	th, guard, archive := startupFixtureServer(t)
	data := bytes.Repeat([]byte("actual attachment evidence\x00"), 110000)
	post, info := uploadFixtureFile(t, th, data)
	guard.mu.Lock()
	guard.held[info.Path] = true
	guard.mu.Unlock()
	require.Nil(t, th.App.PermanentDeleteFilesByPost(th.Context, post.Id, nil))
	exists, appErr := th.App.FileExists(info.Path)
	require.Nil(t, appErr)
	require.False(t, exists, "the native deletion must really remove the live object")
	_, err := th.Store.FileInfo().GetFromMaster(info.Id)
	require.Error(t, err, "the native deletion must really remove FileInfo")
	guard.mu.Lock()
	refs := append([]BlobRef(nil), guard.refs[info.Path]...)
	guard.mu.Unlock()
	require.Len(t, refs, 1)
	require.NoError(t, guard.vault.Close())
	reopened, err := OpenLocalVault(archive)
	require.NoError(t, err)
	defer reopened.Close()
	require.Equal(t, data, readVaultBlob(t, reopened, refs[0]), "independent bytes must survive both native deletions and reopening the vault")
}

// Architectural evidence, not acceptance of the Legal Hold contract. The core
// currently logs object errors and still deletes FileInfo. Therefore the future
// SQL receipt fence must cover this path in addition to the backend guard.
func TestNativePermanentAttachmentDeleteDropsMetadataAfterObjectRefusal(t *testing.T) {
	th, guard, _ := startupFixtureServer(t)
	data := []byte("source must remain after archive failure")
	post, info := uploadFixtureFile(t, th, data)
	guard.mu.Lock()
	guard.held[info.Path] = true
	guard.fault = errors.New("preservation store unavailable")
	guard.mu.Unlock()
	appErr := th.App.PermanentDeleteFilesByPost(th.Context, post.Id, nil)
	// Preserve this observation so an object-only implementation can never be
	// advertised as a complete hold, even though these probe tests pass.
	require.Nil(t, appErr)
	stored, appErr := th.App.ReadFile(info.Path)
	require.Nil(t, appErr)
	require.Equal(t, data, stored)
	_, err := th.Store.FileInfo().GetFromMaster(info.Id)
	require.Error(t, err)
	t.Log("native permanent attachment deletion removed FileInfo after preservation refused object removal; SQL fence required")
}

func TestSharedExportFacadeCannotBypassPreservation(t *testing.T) {
	cases := []objectstoretest.Emulator{{Name: "local"}}
	cases = append(cases, objectstoretest.Cases()...)
	for _, storage := range cases {
		t.Run(storage.Name, func(t *testing.T) {
			var settings *model.FileSettings
			if storage.Setup != nil {
				endpoint := os.Getenv(storage.EndpointEnv)
				if endpoint == "" {
					t.Skip("requires isolated object storage emulator")
				}
				cfg := storage.Setup(t, endpoint)
				settings = &cfg
			}
			th, guard, _ := startupFixtureServerSettings(t, settings)
			data := bytes.Repeat([]byte("shared storage evidence\x00"), 10000)
			_, info := uploadFixtureFile(t, th, data)
			guard.mu.Lock()
			guard.held[info.Path] = true
			guard.mu.Unlock()
			require.Nil(t, th.App.RemoveExportFile(info.Path))
			exists, appErr := th.App.FileExists(info.Path)
			require.Nil(t, appErr)
			require.False(t, exists)
			_, err := th.Store.FileInfo().GetFromMaster(info.Id)
			require.NoError(t, err, "this facade removes only the object, not its FileInfo")
			guard.mu.Lock()
			refs := append([]BlobRef(nil), guard.refs[info.Path]...)
			guard.mu.Unlock()
			require.Len(t, refs, 1)
			require.Equal(t, data, readVaultBlob(t, guard.vault, refs[0]))
		})
	}
}

func TestDedicatedExportBackendRemainsIndependent(t *testing.T) {
	settings := model.FileSettings{}
	settings.SetDefaults(false)
	settings.DedicatedExportStore = model.NewPointer(true)
	settings.ExportDriverName = model.NewPointer(model.ImageDriverLocal)
	settings.ExportDirectory = model.NewPointer(t.TempDir())
	th, guard, _ := startupFixtureServerSettings(t, &settings)
	_, info := uploadFixtureFile(t, th, []byte("live source"))
	guard.mu.Lock()
	guard.held[info.Path] = true
	guard.mu.Unlock()
	_, appErr := th.App.WriteExportFile(bytes.NewReader([]byte("independent export")), info.Path)
	require.Nil(t, appErr)
	live, appErr := th.App.ReadFile(info.Path)
	require.Nil(t, appErr)
	require.Equal(t, []byte("live source"), live)
	guard.mu.Lock()
	defer guard.mu.Unlock()
	require.Empty(t, guard.refs[info.Path], "a distinct export object must never be archived as a held live attachment")
}
