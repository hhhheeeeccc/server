package com.muhammil.videos

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.ContentValues
import android.content.Intent
import android.media.MediaScannerConnection
import android.net.Uri
import android.os.Build
import android.os.Environment
import android.os.IBinder
import android.provider.MediaStore
import android.util.Log
import androidx.core.app.NotificationCompat
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import okhttp3.OkHttpClient
import okhttp3.Request
import java.io.File
import java.io.FileOutputStream
import java.util.concurrent.TimeUnit

/**
 * خدمة التنزيل في المقدمة (Foreground Service).
 *
 * إصلاح المشكلة الأولى: لا شيء يحدث بصمت — كل خطأ يظهر في الإشعار وواجهة التطبيق بنص عربي.
 * إصلاح المشكلة الثانية (إنستغرام): يجرّب المحرك بوضع الزائر أولًا، فإن فشل يستخدم
 * استراتيجيات [InstagramExtractor] بدون أي تسجيل دخول.
 */
class DownloadService : Service() {

    companion object {
        const val EXTRA_URL = "url"
        const val EXTRA_QUALITY = "quality" // best | 720 | mp3
        const val ACTION_STATUS = "com.muhammil.videos.DOWNLOAD_STATUS"
        const val EXTRA_STATUS_TEXT = "text"
        const val EXTRA_STATUS_DONE = "done"
        const val EXTRA_STATUS_PROGRESS = "progress"

        private const val TAG = "DownloadService"
        private const val CHANNEL_ID = "downloads"
        private const val NOTIF_ID = 1
        private const val FOLDER = "محمّل الفيديوهات"
    }

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        val url = intent?.getStringExtra(EXTRA_URL)
        if (url.isNullOrBlank()) {
            stopSelf()
            return START_NOT_STICKY
        }
        val quality = intent.getStringExtra(EXTRA_QUALITY) ?: "best"

        createChannel()
        startForeground(
            NOTIF_ID,
            buildNotification(getString(R.string.notif_title_downloading), -1)
        )

        scope.launch {
            try {
                download(url, quality)
            } catch (e: CancellationException) {
                throw e
            } catch (e: AppException) {
                Log.e(TAG, "download failed (app)", e)
                fail(getString(e.resId))
            } catch (e: Exception) {
                Log.e(TAG, "download failed (unexpected)", e)
                fail(getString(R.string.err_generic))
            }
        }
        return START_NOT_STICKY
    }

    override fun onDestroy() {
        scope.cancel()
        super.onDestroy()
    }

    // ------------------------------------------------------------------
    // مسار التنزيل
    // ------------------------------------------------------------------

    private suspend fun download(url: String, quality: String) {
        val host = Uri.parse(url).host ?: ""
        val isInstagram = host.contains("instagram.com") || host.contains("instagr.am")
        val workDir = File(cacheDir, "engine").apply { mkdirs() }

        if (!isInstagram) {
            finishSave(engineDownload(workDir, url, quality, ::onEngineProgress))
            return
        }

        // إنستغرام: المحرك بوضع الزائر أولًا (بدون أي حساب) — الطبعة الخفيفة تتجاوز هذا تلقائيًا
        try {
            finishSave(engineDownload(workDir, url, quality, ::onEngineProgress))
            return
        } catch (e: CancellationException) {
            throw e
        } catch (e: EngineUnsupportedException) {
            Log.i(TAG, "المحرك الخفيف لا يدعم هذا الرابط — ننتقل للاستراتيجيات اليدوية")
        } catch (e: Exception) {
            Log.w(TAG, "المحرك فشل على إنستغرام، ننتقل للاستراتيجيات اليدوية", e)
        }

        // استراتيجيات يدوية بدون تسجيل دخول
        when (val r = withContext(Dispatchers.IO) { InstagramExtractor.extract(url) }) {
            is InstagramExtractor.IgResult.Video -> {
                if (quality == "mp3") {
                    // تحويل MP3 غير ممكن لهذا المسار — نحفظ MP4 ونوضح ذلك للمستخدم
                    saveDirect(r)
                    postStatus(getString(R.string.share_saved_mp4), done = true)
                } else {
                    saveDirect(r)
                }
            }
            InstagramExtractor.IgResult.NotFound ->
                throw AppException(R.string.err_ig_not_found)
            InstagramExtractor.IgResult.PrivateOrRestricted ->
                throw AppException(R.string.err_ig_private)
            is InstagramExtractor.IgResult.NetworkError ->
                throw AppException(R.string.err_network)
        }
    }

    /** نقل النسبة من المحرك إلى الإشعار وواجهة التطبيق. */
    private fun onEngineProgress(pct: Float) {
        if (pct >= 0) updateNotification(pct)
        postStatus(
            getString(R.string.downloading, pct.toInt().toString()),
            done = false,
            progress = pct
        )
    }

    /** حفظ ملف المحرك في مجلد التنزيلات وإتمام العملية. */
    private fun finishSave(out: EngineOutput) {
        val name =
            sanitizeName(out.title.ifBlank { out.file.nameWithoutExtension }) +
                "." + out.file.extension.lowercase()
        saveToDownloads(out.file, name, out.mime)
        out.file.delete()
        success(name)
    }

    /** تنزيل رابط الفيديو المباشر القادم من استراتيجيات إنستغرام اليدوية. */
    private suspend fun saveDirect(video: InstagramExtractor.IgResult.Video) {
        postStatus(getString(R.string.downloading, "0"), done = false, progress = 0f)
        val temp = File(cacheDir, "ig_${System.currentTimeMillis()}.mp4")
        try {
            val client = OkHttpClient.Builder()
                .connectTimeout(20, TimeUnit.SECONDS)
                .readTimeout(120, TimeUnit.SECONDS)
                .build()
            val req = Request.Builder()
                .url(video.videoUrl)
                .header("User-Agent", InstagramExtractor.UA)
                .header("Referer", "https://www.instagram.com/")
                .build()

            client.newCall(req).execute().use { resp ->
                if (!resp.isSuccessful) throw AppException(R.string.err_ig_generic)
                val body = resp.body ?: throw AppException(R.string.err_ig_generic)
                val total = body.contentLength()
                var written = 0L
                body.byteStream().use { input ->
                    FileOutputStream(temp).use { output ->
                        val buf = ByteArray(64 * 1024)
                        var n = input.read(buf)
                        while (n >= 0) {
                            written += n
                            output.write(buf, 0, n)
                            if (total > 0) {
                                val pct = written * 100f / total
                                updateNotification(pct)
                                if (written % (8L * 1024 * 1024) < buf.size) {
                                    postStatus(
                                        getString(R.string.downloading, pct.toInt().toString()),
                                        done = false,
                                        progress = pct
                                    )
                                }
                            }
                            n = input.read(buf)
                        }
                    }
                }
            }
            val name = sanitizeName(video.title.ifBlank { "instagram_${System.currentTimeMillis()}" }) + ".mp4"
            saveToDownloads(temp, name, "video/mp4")
            success(name)
        } finally {
            temp.delete()
        }
    }

    // ------------------------------------------------------------------
    // الحفظ في مجلد التنزيلات العام
    // ------------------------------------------------------------------

    private fun saveToDownloads(src: File, displayName: String, mime: String): Uri {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            val values = ContentValues().apply {
                put(MediaStore.MediaColumns.DISPLAY_NAME, displayName)
                put(MediaStore.MediaColumns.MIME_TYPE, mime)
                put(
                    MediaStore.MediaColumns.RELATIVE_PATH,
                    Environment.DIRECTORY_DOWNLOADS + "/" + FOLDER
                )
                put(MediaStore.MediaColumns.IS_PENDING, 1)
            }
            val resolver = contentResolver
            val uri = resolver.insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, values)
                ?: throw AppException(R.string.err_generic)
            try {
                resolver.openOutputStream(uri)?.use { out ->
                    src.inputStream().use { it.copyTo(out) }
                } ?: throw AppException(R.string.err_generic)
                val done = ContentValues().apply {
                    put(MediaStore.MediaColumns.IS_PENDING, 0)
                }
                resolver.update(uri, done, null, null)
            } catch (e: Exception) {
                runCatching { resolver.delete(uri, null, null) }
                throw e
            }
            return uri
        } else {
            val dir = File(
                Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_DOWNLOADS),
                FOLDER
            )
            if (!dir.exists()) dir.mkdirs()
            val dest = File(dir, displayName)
            src.copyTo(dest, overwrite = true)
            MediaScannerConnection.scanFile(this, arrayOf(dest.absolutePath), arrayOf(mime), null)
            return Uri.fromFile(dest)
        }
    }

    private fun sanitizeName(name: String): String =
        name.replace(Regex("""[\\/:*?"<>|\n\r\t]"""), " ")
            .replace(Regex("""\s+"""), " ")
            .trim()
            .take(80)
            .ifBlank { "video" }

    // ------------------------------------------------------------------
    // الإشعارات وواجهة المستخدم
    // ------------------------------------------------------------------

    private fun createChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                CHANNEL_ID,
                getString(R.string.notif_channel_name),
                NotificationManager.IMPORTANCE_LOW
            )
            getSystemService(NotificationManager::class.java).createNotificationChannel(channel)
        }
    }

    private fun contentIntent(): PendingIntent =
        PendingIntent.getActivity(
            this, 0,
            Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )

    private fun buildNotification(title: String, progress: Int): Notification {
        val b = NotificationCompat.Builder(this, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_stat_download)
            .setContentTitle(title)
            .setContentText(getString(R.string.notif_in_progress))
            .setOnlyAlertOnce(true)
            .setOngoing(true)
            .setContentIntent(contentIntent())
        if (progress < 0) b.setProgress(0, 0, true)
        else b.setProgress(100, progress, false)
        return b.build()
    }

    private fun updateNotification(progress: Float) {
        val pct = progress.toInt().coerceIn(0, 100)
        getSystemService(NotificationManager::class.java)
            .notify(NOTIF_ID, buildNotification(getString(R.string.notif_title_downloading), pct))
    }

    private fun postStatus(text: String, done: Boolean, progress: Float = -1f) {
        sendBroadcast(
            Intent(ACTION_STATUS).setPackage(packageName)
                .putExtra(EXTRA_STATUS_TEXT, text)
                .putExtra(EXTRA_STATUS_DONE, done)
                .putExtra(EXTRA_STATUS_PROGRESS, progress)
        )
    }

    private fun success(name: String) {
        stopForeground(true)
        val text = getString(R.string.download_success, name)
        val notif = NotificationCompat.Builder(this, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_stat_download)
            .setContentTitle(getString(R.string.notif_title_done))
            .setContentText(text + " — " + getString(R.string.download_success_folder))
            .setAutoCancel(true)
            .setContentIntent(contentIntent())
            .build()
        getSystemService(NotificationManager::class.java).notify(NOTIF_ID + 1, notif)
        postStatus(text, done = true)
        stopSelf()
    }

    private fun fail(message: String) {
        stopForeground(true)
        val notif = NotificationCompat.Builder(this, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_stat_download)
            .setContentTitle(getString(R.string.notif_title_error))
            .setContentText(message)
            .setStyle(NotificationCompat.BigTextStyle().bigText(message))
            .setAutoCancel(true)
            .setContentIntent(contentIntent())
            .build()
        getSystemService(NotificationManager::class.java).notify(NOTIF_ID + 2, notif)
        postStatus(message, done = true)
        stopSelf()
    }
}
