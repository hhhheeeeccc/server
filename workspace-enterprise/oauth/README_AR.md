# موفرا Google وMicrosoft 365

تحديث 20 سبتمبر 2026. `main.go` يسجل `google.New` و`office365.New` في سجل
`OAuthProvider` الأصلي. OpenID الحالي مستقل ولم يُستبدل.

- يتطلب Google ميزة `GoogleOAuth` ويتطلب Microsoft ميزة `Office365OAuth` في
  الترخيص النشط، مع تفعيل المزوّد وإعداد معرّف العميل والسر وروابط HTTPS.
- الخادم الأساسي يتولى حالة OAuth وتبادل رمز التفويض وجلب ملف المستخدم باستخدام
  access token. الحزمتان تقرآن نتيجة واجهة المزوّد؛ لا تستخرجان هوية موثوقة من
  JWT غير متحقق منه. `GetUserFromIdToken` لا يستعمل الادعاءات، وتأتي الهوية من
  طلب People/Graph المصادق عليه بالخادم.
- قارئ ملف People/Graph يرفض جسمًا يتجاوز 1 MiB حتى لو بدأ بكائن JSON صالح
  وتلاه فراغ أو بيانات أخرى. اختُبر حد الحجم ورفض كائن JSON ثانٍ محليًا؛
  نجح `go test -p 2 ./oauth/... -count=1 -timeout=2m` و`go vet ./oauth/...`
  في 1 أكتوبر.
- Google يقرأ `metadata.sources` ويستخدم معرّف `PROFILE`، وبريدًا رئيسيًا موثقًا.
  لا يعتمد على معرّف جهة اتصال أو `resourceName` المتغير.
- Microsoft يقرأ معرّف المستخدم `id` من Graph `/me` والبريد `mail`، أو
  `userPrincipalName` عندما يغيب البريد. `UsePreferredUsername` يدعم اسم UPN.
- `DirectoryId` يحدد دليل Microsoft في رابطَي التفويض والتبادل عند استخدام
  `login.microsoftonline.com`. تُرفض الروابط التي تناقض الدليل المضبوط.
  يشمل التحقق كتابة اسم المضيف بحروف كبيرة أو بنقطة نهائية، لأنهما يشيران
  إلى المضيف نفسه؛ اختُبر إعادة كتابة الرابطين ورفض دليل آخر. نجح
  `go test -p 2 ./oauth/... -count=1 -timeout=2m` و`go vet ./oauth/...`
  في 1 أكتوبر.
- المقارنة تعتمد معرّف الهوية وطريقة الدخول، ولا تعتمد البريد لدمج حسابين.
  الأسماء المحجوزة أو غير المقبولة تحصل على بديل ثابت مشتق من هوية المزوّد.
- لم تتغير ملفات إعدادات المستخدم أو أسرارها، ولم تُنشأ تطبيقات OAuth لدى
  Google/Microsoft. الاختبارات المحلية تغطي الخرائط والإعدادات والرفض والترخيص؛
  تجربة المتصفح مع تطبيق مزوّد حقيقي ما زالت مطلوبة.

المراجع: [Google People](https://developers.google.com/people/api/rest/v1/people)،
[ملف المستخدم المصادق عليه من Microsoft Graph](https://learn.microsoft.com/en-us/graph/api/user-get?view=graph-rest-1.0)،
وواجهة `server/einterfaces/oauthproviders.go` ومسار `AuthorizeOAuthUser` الحاليان.

سجلات التحقق وحدود الدفعة في [التقرير](../audits/2026-09-20/authentication-continuation/REPORT_AR.md).
