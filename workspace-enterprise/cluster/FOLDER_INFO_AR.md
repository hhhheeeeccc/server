# وظيفة مجلد `cluster`

التوافر العالي بين العقد: العضوية، الرسائل، RPC، القيادة، واختبارات العقدتين.

للتفاصيل: [README_AR.md](README_AR.md).

ملفات بارزة: `README_AR.md`، `TWO_NODE_TEST_AR.md`، `doc.go`، `integration_test.go`، `process_integration_test.go`، `persisted_license_integration_test.go`، `database_outage_integration_test.go`، `gossip_partition_integration_test.go`، `cache_recovery.go`، `cache_recovery_test.go`، `cache_crash_integration_test.go`، `cache_recheck_integration_test.go`، `cache_recheck_test.go`، `cache_discovery.go`، `cache_discovery_test.go`، `startup_partition_integration_test.go`، `discovery_failure_integration_test.go`، `cache_periodic_integration_test.go`، `cache_startup_recovery.go`، `cache_startup_recovery_integration_test.go`، `startup_failure_integration_test.go`، `cache_anti_entropy.go`، `cache_anti_entropy_test.go`، `database.go`، `database_test.go`، `rpc.go`، `service.go`.

هذا الوصف يحدد دور المجلد؛ حالة اكتمال الميزة وحدود اختبارها موثقة في [خطة النواقص](../docs/REMAINING_FEATURES_AR.md).

اختبار الترخيص المحفوظ ومسح الكاش السليم موجود؛ حجب SQL الترخيص قد يمنع
الإغلاق والانتقال ويظل غير محلول. [الدليل](../../workspace-audits/workspace-enterprise-2026-10-22/audit/2026-10-18/cluster-persisted-license-shutdown/README_AR.md).
