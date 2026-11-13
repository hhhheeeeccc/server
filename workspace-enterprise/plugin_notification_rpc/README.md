# استعادة اتصال إضافة مكتب الخدمات

يثبت سجل التشغيل خطأ `gob: type not registered for interface:
servicedesk.serviceNotificationCard` عند `CreatePost`، ثم `unexpected EOF`
و`connection is shut down` في `GetUser`. لذلك يظهر خطأ التحقق من الحساب.

التصحيح يحوّل بطاقة الإشعار إلى خرائط وقوائم مسجّلة لدى طرفي RPC، مع حفظ
محتواها العام نفسه. لا يغيّر المصادقة أو الصلاحيات. لا يكفي تسجيل النوع في
الخادم؛ الفشل يحدث أثناء الترميز في عملية الإضافة قبل وصول الطلب إليه.

`prepare.py` ينسخ مصدر خادم الإضافة 0.39.3 إلى لقطة بناء تحت Enterprise
ويطبّق التصحيح فيها. لا يكتب في مصدر Mattermost أو مصدر الإضافة الأصلي.
اختبار الانحدار يمرّر بيانات البطاقة عبر `Z_CreatePostArgs` بترميز Gob الفعلي،
ويتحقق من بقاء JSON نفسه، بما فيه حقول الطلب وأسماء الأشخاص.

```bash
python3 plugin_notification_rpc/prepare.py --source '/path/to/workspace-tickets' \
  --output "$PWD/artifacts/plugin-notification-rpc/source"
cd artifacts/plugin-notification-rpc/source
GOWORK=off go test ./server/internal/servicedesk -run 'Test(NotificationPropsSurvivePluginRPC|ServiceNotification)' -count=1
GOWORK=off CGO_ENABLED=0 go build -trimpath -o ../plugin-linux-amd64 ./server
```
