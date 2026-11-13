# وظيفة مجلد `message_export`

تصدير الرسائل المجدول واليدوي، استئناف الدفعات، المرفقات، وصيغ التصدير المختلفة.

للتفاصيل: [README_AR.md](README_AR.md).

ملكية المحاولات واستدراك السقوط: `claim_owner.go`، `claim_session.go`،
`claim_socket.go`، واختبارات الجلسة والعملية المنفصلة؛ حدود التخزين الخارجي
والنشر موثقة في README أعلاه.

قراءات العامل المحدودة: `read_session.go` و`read_store.go`، واختبارات
إلغاء SQL ومطابقة المخزن الأصلي؛ لا تستبدل مخزن التطبيق ولا مسار CLI القديم.

ملفات بارزة: `README_AR.md`، `doc.go`، `file_deletions.go`، `integration_test.go`، `membership_map.go`، `membership_map_test.go`، `message_export.go`، `object_store_integration_test.go`، وغيرها.

هذا الوصف يحدد دور المجلد؛ حالة اكتمال الميزة وحدود اختبارها موثقة في [خطة النواقص](../docs/REMAINING_FEATURES_AR.md).
