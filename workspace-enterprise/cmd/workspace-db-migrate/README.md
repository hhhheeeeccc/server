# Workspace database migration

## مجريشن الترخيص عند بدء المشروع

إصلاح قيد الترخيص مربوط مباشرة ببدء نسخة Enterprise في `../../main.go`،
داخل `app.RegisterComplianceInterface` بعد تهيئة مخزن SQL وقبل إتاحة الواجهات. يستخدم
`database_migration.RepairLicenseID` واتصال قاعدة البيانات الخاص بالمشروع،
دون تثبيت اسم `sofa_backup` أو أي اسم قاعدة آخر في الكود.

بعد إعادة بناء الخادم وتشغيله، يفحص قيد `licenses.id` ويضيفه إذا كان مفقودًا.
إذا كان موجودًا، لا يغيره. لا يلزم تشغيل أداة المجريشن يدويًا لهذا الإصلاح.
لا ينشئ ترخيصًا ولا يغير محتوى سجلاته أو يتجاوز التحقق من توقيعه.

أداة الأوامر أدناه مخصصة للصيانة وفحص البنية الكامل؛ لا تحتاج تشغيلها
للاستفادة من مجريشن الترخيص التلقائي. تعديل المصدر وحده لا يحدّث ملف
الخادم التنفيذي الجاري.

## مجريشن بريد المستخدمين عند بدء المشروع

يعالج `database_migration/user_email.go` فشل تقارير المستخدمين عند قراءة بريد
بقيمة NULL. يُستدعى من `user_no_email.Register` أثناء بدء Enterprise المسجل
في `main.go`، قبل إتاحة واجهات المستخدمين.

- يحول فهارس البريد الفريدة الكاملة، ومنها `idx_users_email` القديمة، إلى
  فهارس جزئية تستثني البريد الفارغ، مع الحفاظ على تفرد العناوين الحقيقية.
- يحول NULL إلى نص فارغ، ويضبط `DEFAULT ''` و`NOT NULL` لمنع عودة المشكلة.
- يعمل داخل معاملة واحدة؛ يرفض العناوين الحقيقية المكررة والقيود المتعارضة
  دون حذف حسابات أو فك علاقات بمفتاح خارجي.
- يتكرر دون تغيير بيانات سليمة، ولا يعتمد على اسم قاعدة محدد.

هذا الربط يحتاج إعادة بناء الخادم وتشغيله. تفاصيل الاختبارات المعزولة:
[`../../audit/2026-10-17/user-report-migration/README_AR.md`](../../../workspace-audits/workspace-enterprise-2026-10-22/audit/2026-10-17/user-report-migration/README_AR.md).

## مجريشن المفتاح الأساسي لسياسات الاحتفاظ

تستدعي تهيئة `app.RegisterDataRetentionInterface` في `main.go` الدالة
`database_migration.RepairRetentionPolicyID` قبل إنشاء خدمة سياسات الاحتفاظ.
تعيد `PRIMARY KEY (id)` المفقود من `retentionpolicies`، وهو مطلوب لاستعلام
قائمة السياسات الذي يستخدم GROUP BY؛ الفهرس الفريد وحده لا يكفي لهذا الاستعلام.
إذا كان المفتاح الصحيح موجودًا فلا تغير شيئًا. المعرّفات المكررة أو NULL،
والمفتاح المختلف أو المؤجل، توقف الإصلاح دون تغيير السياسات أو علاقاتها.

يعمل تلقائيًا بعد إعادة بناء الخادم وتشغيله، دون تثبيت اسم قاعدة في الكود.
لم يُطبق على قاعدة العمل أثناء التطوير. تفاصيل التحقق:
[`../../audit/2026-10-17/retention-policy-migration/README_AR.md`](../../../workspace-audits/workspace-enterprise-2026-10-22/audit/2026-10-17/retention-policy-migration/README_AR.md).

## مجريشن إعداد مجلد السجلات

تعالج حزمة `logging_migration` إعداد `/logs` القديم عند بدء Enterprise،
باستخدام مجلد السجلات المسموح للكور، عبر حفظ الإعداد المعتاد دون تعديل
قواعد البيانات أو توسيع صلاحيات القراءة. يتطلب إعادة بناء الخادم وتشغيله.
التفاصيل والاختبارات:
[`../../logging_migration/README_AR.md`](../../logging_migration/README_AR.md).

## مجريشن حقول التحكم بالوصول عند بدء المشروع

تستدعي تهيئة Enterprise في `main.go` الدالة
`database_migration.RepairPropertyFieldUniqueness` بعد إنشاء SQL وقبل إتاحة
الواجهات. تستبدل القيد المستعاد `propertyfields_groupid_name_unique` الذي
يسبب `23505` عند إنشاء حقل نظام بجانب قالب بالاسم نفسه، بفهرسي الكور اللذين
يراعيان نوع الحقل ونطاقه. تعمل بمعاملة واحدة دون تعديل الحقول أو قيمها،
وترفض التعريفات غير المتوقعة. لا تحتاج تشغيل أداة صيانة يدوية لهذا الإصلاح.

لم يُطبق على قاعدة العمل أثناء التطوير؛ يحتاج إعادة بناء الخادم وتشغيله.
التفاصيل ونتائج الاختبارات المعزولة:
[`../../audit/2026-10-17/property-field-migration/README_AR.md`](../../../workspace-audits/workspace-enterprise-2026-10-22/audit/2026-10-17/property-field-migration/README_AR.md).

## مجريشن حفظ قيم التحكم بالوصول عند بدء المشروع

تستدعي تهيئة Enterprise في `main.go` الدالة
`database_migration.RepairPropertyValueUniqueness` بعد إنشاء SQL وقبل إتاحة
الواجهات. تعالج `42P10` في حفظ القيم باستعادة الفهرس الفريد الجزئي على
`(groupid,targetid,fieldid) WHERE deleteat=0` إذا لم يوجد فهرس صالح للاستعلام.
تعمل بمعاملة واحدة دون تعديل القيم أو حذف التكرارات، وتتوقف عند البيانات
المتعارضة أو القيود المؤجلة أو تصادم الأسماء.

يلزم إعادة بناء الخادم وتشغيله؛ لا تحتاج أداة صيانة يدوية لهذا الإصلاح.
لم يُطبق على قاعدة العمل أثناء التطوير. التفاصيل والاختبارات:
[`../../audit/2026-10-17/property-value-migration/README_AR.md`](../../../workspace-audits/workspace-enterprise-2026-10-22/audit/2026-10-17/property-value-migration/README_AR.md).

## مجريشن تعديل القنوات العامة عند بدء المشروع

توجد أيضًا معالجة تلقائية لفشل تعديل القنوات العامة `42P10` في
`database_migration.RepairPublicChannelID`، تستدعيها تهيئة Enterprise في
`main.go` بعد إنشاء SQL وقبل الواجهات. تعيد قيدًا فريدًا صالحًا على
`publicchannels.id` إذا غاب، دون حذف القنوات أو تغيير المحتوى والعضويات.
ترفض القيم المتعارضة والقيود المؤجلة. تتطلب إعادة بناء الخادم وتشغيله، ولا
تحتاج تشغيل هذه الأداة يدويًا لهذا الإصلاح. لم تُطبق على قاعدة العمل أثناء التطوير.

التفاصيل ونتائج الاختبارات:
[`../../audit/2026-10-17/public-channel-migration/README_AR.md`](../../../workspace-audits/workspace-enterprise-2026-10-22/audit/2026-10-17/public-channel-migration/README_AR.md).

## مجريشن حفظ الرسائل المؤقتة عند بدء المشروع

تستدعي تهيئة Enterprise في `main.go` الدالة
`database_migration.RepairTemporaryPostID` بعد إنشاء SQL وقبل إتاحة الواجهات.
تعالج `42P10` عند إرسال رسالة تحذف بعد القراءة باستعادة قيد فريد صالح على
`temporaryposts.postid` عند غيابه، دون تغيير الرسائل أو المرفقات أو أوقات
الانتهاء أو إيصالات القراءة. ترفض التكرارات وNULL والقيود المؤجلة وتصادم الأسماء.

تحتاج إعادة بناء الخادم وتشغيله؛ لا تحتاج تشغيل هذه الأداة يدويًا لهذا الإصلاح.
لم تُطبق على قاعدة العمل أثناء التطوير. التفاصيل والاختبارات:
[`../../audit/2026-10-17/temporary-post-migration/README_AR.md`](../../../workspace-audits/workspace-enterprise-2026-10-22/audit/2026-10-17/temporary-post-migration/README_AR.md).

## Full database maintenance tool

Full Arabic deployment guide and verification details:
[`../../deployment/database/README_AR.md`](../../deployment/database/README_AR.md).

From the Enterprise project:

```sh
go build -o dist/workspace-db-migrate ./cmd/workspace-db-migrate
./dist/workspace-db-migrate -config /absolute/path/to/config.json -database sofa
```

Run with the target application stopped and a full backup available. This is a
write operation. It validates/reconciles known branch history, runs the core
pre-migrations and pending migrations, restores missing core indexes/keys from
a source-bound fresh-schema reference, and archives/normalizes restored NULL
file metadata. Repetition is safe; unknown history, conflicting indexes,
duplicate keys or incomplete schemas fail explicitly without deleting records.

The additive schema repair is transactional; core migrations, history repair
and metadata normalization have separate transaction boundaries. Existing
partial email uniqueness is preserved. Plugin schema migration remains owned
by the corresponding plugin.

Use the provided `deployment/database/workspace-enterprise.service.d/migrations.conf`
as an `ExecStartPre` drop-in at deployment time. It is not installed by building
this project. The full schema repairs require this tool. Since 2026-10-17,
the Enterprise startup registration in `main.go` also runs the narrow license
ID repair after SQL store creation and before APIs are exposed (requires rebuilding the server).

To test on a disposable PostgreSQL 16 cluster and rebuild the verified binary:

```sh
python3 deployment/database/verify-local.py
```

The reference must be regenerated on a fresh isolated database whenever core
migration SQL changes; see the Arabic guide. The tool rejects stale references.

`-repair-license-id-only` retains the narrow legacy maintenance operation and
does not run core migrations, full schema repair or file metadata normalization.
