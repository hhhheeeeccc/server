# وظيفة مجلد `autotranslation`

تكامل الترجمة التلقائية وتخزين نتائجها، مع حدود إرسال النصوص ومفاتيح المزود الخارجي.

للتفاصيل: [README_AR.md](README_AR.md).

ملفات بارزة: `README_AR.md`، `service.go`، `service_integration_test.go`، `service_test.go`، `libretranslate_integration_test.go`.

فحص المزوّد الحقيقي ووصفة تشغيله بمجزّئ spaCy موجودان في
[`deployment/autotranslation`](../deployment/autotranslation/README_AR.md).

مهلة المزوّد مشتركة بين اكتشاف اللغة ولغات المحاولة الأمامية؛ يحفظ
`service.go` اللغات غير المكتملة للاستعادة، وتغطي اختبارات
`provider_budget_integration_test.go` زمن الانتظار ومساري REST وWebSocket
واستعادة الصفوف في خدمة جديدة.

هذا الوصف يحدد دور المجلد؛ حالة اكتمال الميزة وحدود اختبارها موثقة في [خطة النواقص](../docs/REMAINING_FEATURES_AR.md).

يحفظ `publication.go` علامة نشر نتيجة الاستعادة مع حجزها وتأكيدها، ويصرفها
بعد سقوط الناشر دون طلب ترجمة جديد. يجهز `recovery_index.go` فهرسي الاستدلال
والنشر؛ تغطي اختبارات `publication_*` قتل العملية والتزامن وفشل التأكيد
وفهرس الطابور. يبقى ACK المتصفح وانقطاعه منفصلين عن تأكيد dispatch المحلي.
