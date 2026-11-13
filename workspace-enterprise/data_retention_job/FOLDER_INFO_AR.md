# وظيفة مجلد `data_retention_job`

عامل حذف البيانات المنتهية وفق سياسات الاحتفاظ، مع اختبارات الملفات والمخازن.

للتفاصيل: [README_AR.md](README_AR.md).

ملفات بارزة: `README_AR.md`، `service.go`، `service_integration_test.go`، `service_test.go`، `execution_store.go`،
`execution_store_integration_test.go`، `session_loss_integration_test.go`،
`file_completion.go`، `file_completion_integration_test.go`، `object_loss_integration_test.go`،
`file_removal.go`، `ambiguous_reply_integration_test.go`، `process_recovery_integration_test.go`.

`job_recovery.go` و`job_recovery_integration_test.go`: استعادة محاولات المهام
المعروفة بعد التعطل، وحماية حالات المهمة من عامل محاولة قديمة.

`execution_socket.go` و`execution_deadline_test.go` و
`execution_deadline_integration_test.go`: قطع مآخذ جلسة SQL الخاصة عند
المهلة أو الإلغاء، واختبار المصافحة وطلب إلغاء pq الصامتين وتحرير قفل
حذف محجوب. لا تشمل الحماية مجموعة اتصالات التطبيق أو طلب تخزين جارٍ.

هذا الوصف يحدد دور المجلد؛ حالة اكتمال الميزة وحدود اختبارها موثقة في [خطة النواقص](../docs/REMAINING_FEATURES_AR.md).
