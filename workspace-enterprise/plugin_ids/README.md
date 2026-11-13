# معرّفات الإضافات وإزالة الإضافات المحذوفة

## التحزيم بمعرّفات Workspace دون تعديل ملفات server

من مجلد `mattermost/server`:

```bash
python3 ../../workspace-enterprise/plugin_ids/prepare_overlay.py
go build -overlay ../../workspace-enterprise/plugin_ids/build_overlay/overlay.json \
  -tags 'enterprise sourceavailable' \
  -ldflags '-X github.com/mattermost/mattermost/server/public/model.BuildEnterpriseReady=true' \
  -o bin/mattermost ./cmd/mattermost
```

`-overlay` يحتاج ملف JSON؛ لا يكفي كتابة الخيار وحده. المولّد يقرأ الثوابت
الأصلية ويكتب البديل وملف JSON داخل Enterprise فقط. يعيد تسمية Playbooks إلى
`workspace`، وApps وCalls وNPS إلى `com.workspace.*`، وAI إلى `workspace-ai`.
يبقي Focalboard ومعرّف تصدير القنوات كما هما. الملفات الأصلية لا تتغير.
أعد تشغيل المولّد عند نقل المشروع أو تحديث الثوابت الأصلية؛ ملف JSON يحوي
مسارات مطلقة. البديل ينتهي بـ `.go.txt` لكي لا يُعامل كحزمة Go مستقلة.

أمر البناء العادي دون `-overlay` يستخدم الأسماء الأصلية. النسخة الناتجة بالـ
overlay تحتفظ بالمعرّفات الجديدة عند نقلها إلى جهاز آخر دون الحاجة لملف JSON
أثناء التشغيل. إعدادات قديمة موجودة لا تُحذف تلقائيًا. لا تغيّر هذه العملية
هوية حزمة إضافة فعلية؛ يجب أن يطابق معرّف `plugin.json` معرّفها في الإعدادات.

للتحقق من البناء والإعدادات الجديدة دون تشغيل الخادم أو الاتصال بالقاعدة:

```bash
go test -overlay ../../workspace-enterprise/plugin_ids/build_overlay/overlay.json \
  ../../workspace-enterprise/plugin_ids ../../workspace-enterprise/cmd/workspace-config
```

## سلوك البناء العادي دون overlay

وجود `PluginStates[id].Enable` لا يثبت أن الإضافة مثبتة. الكور ينشئ مفاتيح
Calls وNPS وPlaybooks وAI افتراضيًا عند غيابها من الإعدادات.

كان تحويل أسماء الإضافات ينسخ حالات التفعيل إلى أسماء Workspace أيضًا، حتى
عندما لا توجد حزمة الإضافة. الآن ينقل `MigratePluginSettings` الخيارات فقط
لإضافة Workspace ذات حالة محفوظة مسبقًا؛ لا ينشئ حالة جديدة ولا يفعّل إضافة.

لإبقاء إضافات الكور المحذوفة معطّلة، أبق مفاتيحها مع `Enable: false`.
حذف المفاتيح يجعل `SetDefaults` يعيد إنشاءها. يمكن حذف حالات الأسماء البديلة
مثل `com.workspace.calls` و`workspace-ai` عندما تكون إضافاتها محذوفة.
لا يحذف هذا الكود ملفات الإضافات أو خياراتها المحفوظة.

عند التشغيل، إذا كان `AutomaticPrepackagedPlugins: false`، يفحص `Register`
معرّفات الإضافات في ملفات `plugin.json` داخل مجلد الإضافات. يجعل حالات إضافات
الكور الأربع غير المثبتة `false` ويحفظها إن كانت الإعدادات قابلة للكتابة؛
يحافظ على حالات الإضافات المثبتة. عند تعذر قراءة الملفات يترك الحالات كما هي
ويكتب تحذيرًا في السجل. لذلك لا يعيد حذف مفاتيح الإضافات غير المثبتة تفعيلها
في نسخة Enterprise الجديدة عندما يكون التثبيت التلقائي معطّلًا.

لا تحفظ نسخ رجوع لحزم الإضافات داخل مخزن `data/plugins`؛ الخادم يفحص ملفات
`tar.gz` الموجودة فيه عند المزامنة، وقد يعيد تثبيت إضافة حذفتها.

مولّد `cmd/workspace-config` يولّد إعدادات توزيع جديدة مع تعطيل الإضافات
الافتراضية و`AutomaticPrepackagedPlugins`. ذلك يمنع تثبيت إضافات مرفقة بالحزمة
تلقائيًا؛ يمكن للمسؤول تثبيت الإضافات المطلوبة وتفعيلها صراحةً.

اختبار العودة إلى القيم الافتراضية والتأكد من عدم إعادة إنشاء أسماء Workspace:

```bash
# من mattermost/server
go test ../../workspace-enterprise/plugin_ids ../../workspace-enterprise/cmd/workspace-config
```

بعد تعديل كود Enterprise يلزم بناء ملف الخادم مجددًا، ثم تشغيل النسخة الجديدة.
