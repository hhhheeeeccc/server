# VideoLoad Android (Kotlin)

هذه نسخة Android Native مبنية بـ Kotlin من تطبيق VideoLoad.

## الوظائف الحالية

- واجهة عربية بسيطة.
- استقبال رابط عبر المشاركة في Android أو فتح رابط من المتصفح.
- فحص روابط الوسائط المباشرة.
- تنزيل الملفات المسموح بها عبر `DownloadManager` إلى مجلد `Downloads`.
- دعم MP4 وM4V وMOV وWEBM وMKV وAVI وMP3 وM4A وAAC وWAV.

## حدود الاستخدام

هذه النسخة لا تستخرج ملفات من صفحات YouTube أو Instagram أو Snapchat أو TikTok، ولا تتجاوز DRM أو تسجيل الدخول أو حماية المنصات. رابط صفحة المشاهدة ليس رابط ملف مباشرًا. استخدم فقط المواد التي تملك حق تنزيلها.

## البناء

افتح مجلد `android/` في Android Studio ثم نفّذ Gradle Sync وابنِ APK من `app`.

يتطلب البناء Android SDK 35 وJDK 17 أو أحدث. لم يتم تضمين APK جاهز داخل المستودع.
