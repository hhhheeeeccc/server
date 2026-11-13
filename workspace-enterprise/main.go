package enterprise

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/mattermost/enterprise/access_control"
	"github.com/mattermost/enterprise/access_control_job"
	"github.com/mattermost/enterprise/account_linking"
	"github.com/mattermost/enterprise/account_migration"
	"github.com/mattermost/enterprise/agents_bridge"
	"github.com/mattermost/enterprise/audit_file"
	"github.com/mattermost/enterprise/auth_verify"
	"github.com/mattermost/enterprise/autotranslation"
	"github.com/mattermost/enterprise/brand"
	"github.com/mattermost/enterprise/browser_sentry"
	"github.com/mattermost/enterprise/bulk_reactions"
	"github.com/mattermost/enterprise/buzz"
	"github.com/mattermost/enterprise/channel_last_post"
	"github.com/mattermost/enterprise/channel_leave_policy"
	"github.com/mattermost/enterprise/channel_management"
	"github.com/mattermost/enterprise/channel_read_receipts"
	"github.com/mattermost/enterprise/cloud"
	"github.com/mattermost/enterprise/cluster"
	"github.com/mattermost/enterprise/compliance_service"
	"github.com/mattermost/enterprise/content_extraction"
	"github.com/mattermost/enterprise/data_retention"
	"github.com/mattermost/enterprise/data_retention_job"
	"github.com/mattermost/enterprise/database_migration"
	"github.com/mattermost/enterprise/desktop_app_page"
	"github.com/mattermost/enterprise/disappearing_messages"
	"github.com/mattermost/enterprise/elasticsearch_indexing"
	"github.com/mattermost/enterprise/file_metadata_repair"
	"github.com/mattermost/enterprise/file_storage"
	"github.com/mattermost/enterprise/inactive_users"
	"github.com/mattermost/enterprise/intune"
	"github.com/mattermost/enterprise/ip_filtering"
	"github.com/mattermost/enterprise/ldap"
	"github.com/mattermost/enterprise/license"
	"github.com/mattermost/enterprise/localization"
	"github.com/mattermost/enterprise/logging_migration"
	"github.com/mattermost/enterprise/message_export"
	"github.com/mattermost/enterprise/notification"
	"github.com/mattermost/enterprise/nova_sms"
	"github.com/mattermost/enterprise/oauth/google"
	"github.com/mattermost/enterprise/oauth/office365"
	"github.com/mattermost/enterprise/oauth/openid"
	"github.com/mattermost/enterprise/online_users"
	"github.com/mattermost/enterprise/openid_logout"
	"github.com/mattermost/enterprise/outgoing_oauth_connections"
	"github.com/mattermost/enterprise/partial_search"
	"github.com/mattermost/enterprise/phone_attribute"
	"github.com/mattermost/enterprise/phone_password_reset"
	"github.com/mattermost/enterprise/plugin_assets"
	"github.com/mattermost/enterprise/plugin_ids"
	"github.com/mattermost/enterprise/post_ack_batch"
	"github.com/mattermost/enterprise/post_forwarding"
	"github.com/mattermost/enterprise/preferences"
	"github.com/mattermost/enterprise/product_notices"
	"github.com/mattermost/enterprise/profile_gif"
	"github.com/mattermost/enterprise/push_proxy"
	"github.com/mattermost/enterprise/required_password_change"
	"github.com/mattermost/enterprise/saml"
	"github.com/mattermost/enterprise/schema_migration"
	"github.com/mattermost/enterprise/server_sentry"
	"github.com/mattermost/enterprise/site_links"
	"github.com/mattermost/enterprise/slash_commands"
	"github.com/mattermost/enterprise/status_lock"
	"github.com/mattermost/enterprise/step_up_mfa"
	"github.com/mattermost/enterprise/team_deletion"
	"github.com/mattermost/enterprise/terms_of_service"
	"github.com/mattermost/enterprise/user_lookup"
	"github.com/mattermost/enterprise/user_no_email"
	"github.com/mattermost/enterprise/video_thumbnails"
	"github.com/mattermost/enterprise/visual_editor"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/app/platform"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	ejobs "github.com/mattermost/mattermost/server/v8/einterfaces/jobs"
	_ "github.com/mattermost/mattermost/server/v8/enterprise/elasticsearch" // register the built-in search engine before overriding its index job
)

var openIDProvider = openid.New()

var oauthApp atomic.Pointer[app.App]

func oauthLicense() *model.License {
	if a := oauthApp.Load(); a != nil {
		return a.License()
	}
	return nil
}

func init() {
	if err := product_notices.Disable(); err != nil {
		panic(err)
	}
	if err := localization.RegisterArabicLocale(); err != nil {
		panic(err)
	}
	utils.LicenseValidator = license.NewValidator()
	server_sentry.DisableDefault()
	openIDProvider.Register()
	einterfaces.RegisterOAuthProvider(model.ServiceGoogle, google.New(oauthLicense))
	einterfaces.RegisterOAuthProvider(model.ServiceOffice365, office365.New(oauthLicense))
	channel_leave_policy.RegisterCommands()
	slash_commands.Register()
	status_lock.RegisterCommands()
	disappearing_messages.RegisterCommands()
	disappearing_messages.RegisterPermissions()
	platform.RegisterClusterInterface(cluster.New)
	app.RegisterCloudInterface(cloud.New)
	app.RegisterPushProxyInterface(push_proxy.New)

	platform.RegisterLicenseInterface(func(p *platform.PlatformService) einterfaces.LicenseInterface {
		schema_migration.Register(p)
		return license.New()
	})

	app.RegisterComplianceInterface(func(a *app.App) einterfaces.ComplianceInterface {
		logging_migration.Register(a)
		// Platform license registration runs before its SQL store is created.
		// Run the store repair here, after platform startup and before APIs.
		if db := a.Srv().Store().GetInternalMasterDB(); db != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
			changed, err := database_migration.RepairLicenseID(ctx, db)
			cancel()
			if err != nil {
				a.Log().Error("Failed to repair license ID constraint", mlog.Err(err))
			} else if changed {
				a.Log().Info("Restored license ID constraint; existing license records preserved")
			}
			ctx, cancel = context.WithTimeout(context.Background(), 35*time.Second)
			changed, err = database_migration.RepairPropertyFieldUniqueness(ctx, db)
			cancel()
			if err != nil {
				a.Log().Error("Failed to repair property field uniqueness", mlog.Err(err))
			} else if changed {
				a.Log().Info("Restored typed property field uniqueness; existing fields preserved")
			}
			ctx, cancel = context.WithTimeout(context.Background(), 35*time.Second)
			changed, err = database_migration.RepairPropertyValueUniqueness(ctx, db)
			cancel()
			if err != nil {
				a.Log().Error("Failed to repair property value conflict index", mlog.Err(err))
			} else if changed {
				a.Log().Info("Restored property value conflict index; existing values preserved")
			}
			ctx, cancel = context.WithTimeout(context.Background(), 35*time.Second)
			changed, err = database_migration.RepairPublicChannelID(ctx, db)
			cancel()
			if err != nil {
				a.Log().Error("Failed to repair public channel ID constraint", mlog.Err(err))
			} else if changed {
				a.Log().Info("Restored public channel ID constraint; existing channels preserved")
			}
			ctx, cancel = context.WithTimeout(context.Background(), 35*time.Second)
			changed, err = database_migration.RepairTemporaryPostID(ctx, db)
			cancel()
			if err != nil {
				a.Log().Error("Failed to repair temporary post ID constraint", mlog.Err(err))
			} else if changed {
				a.Log().Info("Restored temporary post ID constraint; existing messages and expiry times preserved")
			}
			ctx, cancel = context.WithTimeout(context.Background(), 125*time.Second)
			changed, err = database_migration.RepairJobIndexes(ctx, db)
			cancel()
			if err != nil {
				a.Log().Error("Failed to restore Jobs lookup indexes", mlog.Err(err))
			} else if changed {
				a.Log().Info("Restored Jobs lookup indexes; existing jobs preserved")
			}
			ctx, cancel = context.WithTimeout(context.Background(), 125*time.Second)
			changed, err = database_migration.RepairThreadMembershipIndexes(ctx, db)
			cancel()
			if err != nil {
				a.Log().Error("Failed to restore thread membership lookup indexes", mlog.Err(err))
			} else if changed {
				a.Log().Info("Restored thread membership lookup indexes; existing memberships preserved")
			}
			ctx, cancel = context.WithTimeout(context.Background(), 35*time.Second)
			var database string
			err = db.QueryRowContext(ctx, "SELECT current_database()").Scan(&database)
			if err == nil {
				var report file_metadata_repair.Report
				report, err = file_metadata_repair.Run(ctx, db, database, true)
				if err == nil && report.Applied {
					a.Log().Info("Normalized restored optional file metadata; original values archived",
						mlog.Any("rows", report.Affected), mlog.String("backup_table", report.BackupTable))
				}
			}
			cancel()
			if err != nil {
				a.Log().Error("Failed to normalize restored optional file metadata", mlog.Err(err))
			}
			ctx, cancel = context.WithTimeout(context.Background(), 35*time.Second)
			err = autotranslation.InstallPostLifecycle(ctx, db)
			cancel()
			if err != nil {
				// Serving deletion APIs without this contract would retain derived
				// private text after deleting its source. Do not start partially.
				panic(fmt.Errorf("install post translation lifecycle: %w", err))
			}
		}

		autotranslation.RegisterHistory(a)
		agents_bridge.Register(a)
		server_sentry.SetLicenseCheck(func() bool { return a.License() != nil })
		if err := server_sentry.Configure(a.License() != nil); err != nil {
			mlog.Warn("Invalid Enterprise server Sentry configuration; reporting disabled", mlog.Err(err))
		}
		account_migration.RegisterAPI(a)
		oauthApp.Store(a)
		openIDProvider.Bind(a)
		// Migrate restored NULL emails and legacy email uniqueness before user APIs start.
		noEmailService := user_no_email.Register(a)
		openIDProvider.BindEmailProvisioner(noEmailService)
		account_linking.Register(a)
		openid_logout.Register(a)
		audit_file.Register(a)
		auth_verify.Register(a)
		brand.Register(a)
		site_links.Register(a)
		browser_sentry.Register(a)
		bulk_reactions.Register(a)
		channel_management.Register(a)
		channel_leave_policy.Register(a)
		channel_last_post.Register(a)
		channel_read_receipts.Register(a)
		disappearing_messages.Register(a)
		file_storage.Register(a)
		desktop_app_page.Register(a)
		inactive_users.Register(a)
		nova := nova_sms.Register(a)
		phone_password_reset.Register(a, nova)
		localization.Register(a)
		buzz.Register(a)
		online_users.Register(a)
		preferences.Register(a)
		profile_gif.Register(a)
		required_password_change.Register(a)
		partial_search.Register(a)
		phone_attribute.Register(a)
		plugin_ids.Register(a)
		plugin_assets.Register(a)
		post_ack_batch.Register(a)
		post_forwarding.Register(a)
		status_lock.Register(a)
		step_up_mfa.Register(a)
		team_deletion.Register(a)
		terms_of_service.Register(a)
		user_lookup.Register(a)
		video_thumbnails.Register(a)
		visual_editor.Register(a, "config/workspace-visual-editor.json")
		return compliance_service.New(a)
	})

	app.RegisterDataRetentionInterface(func(a *app.App) einterfaces.DataRetentionInterface {
		// The recorded core migration does not recreate a primary key lost
		// during restore. Grouped policy queries require a real primary key.
		if db := a.Srv().Store().GetInternalMasterDB(); db != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
			changed, err := database_migration.RepairRetentionPolicyID(ctx, db)
			cancel()
			if err != nil {
				a.Log().Error("Unable to migrate retention policy primary key", mlog.Err(err))
			} else if changed {
				a.Log().Info("Restored retention policy primary key; policies preserved")
			}
		}
		return data_retention.New(a)
	})
	app.RegisterIPFilteringInterface(func(a *app.App) einterfaces.IPFilteringInterface {
		// This constructor runs after initJobs and before StartWorkers, so the
		// native extraction worker is replaced before either worker can run.
		content_extraction.Register(a)
		return ip_filtering.New(a)
	})

	app.RegisterJobsDataRetentionJobInterface(func(server *app.Server) ejobs.DataRetentionJobInterface {
		return data_retention_job.New(server)
	})

	app.RegisterJobsMessageExportJobInterface(func(server *app.Server) ejobs.MessageExportJobInterface {
		return message_export.NewJob(server)
	})
	app.RegisterJobsElasticsearchIndexerInterface(elasticsearch_indexing.New)

	app.RegisterLdapInterface(func(a *app.App) einterfaces.LdapInterface {
		return ldap.New(a)
	})
	app.RegisterJobsLdapSyncInterface(ldap.NewSyncJob)
	app.RegisterSamlInterface(saml.New)
	platform.RegisterSamlDiagnosticInterface(saml.NewDiagnostic)
	app.RegisterIntuneInterface(intune.New)
	app.RegisterAccountMigrationInterface(account_migration.New)

	platform.RegisterLdapDiagnosticInterface(func(ps *platform.PlatformService) einterfaces.LdapDiagnosticInterface {
		return ldap.NewDiagnostics(ps)
	})

	app.RegisterNotificationInterface(func(a *app.App) einterfaces.NotificationInterface {
		return notification.New(a)
	})

	app.RegisterMessageExportInterface(func(a *app.App) einterfaces.MessageExportInterface {
		return message_export.New(a)
	})

	app.RegisterOutgoingOAuthConnectionInterface(func(a *app.App) einterfaces.OutgoingOAuthConnectionInterface {
		return outgoing_oauth_connections.New(a)
	})

	app.RegisterAutoTranslationInterface(func(server *app.Server) einterfaces.AutoTranslationInterface {
		// Server-level Enterprise constructors run before Platform.Start and
		// before user services capture stores. Never replace the store from a
		// later Channels constructor while WebSocket hubs are already reading it.
		account_migration.RegisterPrimarySessionLookupsBeforeStart(server)
		autotranslation.RegisterPostWritesBeforeStart(server)
		return autotranslation.New(server)
	})

	app.RegisterAccessControlServiceInterface(func(a *app.App) einterfaces.AccessControlServiceInterface {
		return access_control.New(a)
	})

	app.RegisterJobsAccessControlSyncJobInterface(func(server *app.Server) ejobs.AccessControlSyncJobInterface {
		return access_control_job.NewChannel(server)
	})

	app.RegisterJobsAccessControlTeamSyncJobInterface(func(server *app.Server) ejobs.AccessControlSyncJobInterface {
		return access_control_job.NewTeam(server)
	})
}
