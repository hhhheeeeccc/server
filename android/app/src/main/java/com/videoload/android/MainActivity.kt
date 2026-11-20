package com.videoload.android

import android.app.DownloadManager
import android.content.Context
import android.content.Intent
import android.graphics.Color
import android.net.Uri
import android.os.Bundle
import android.os.Environment
import android.view.Gravity
import android.view.ViewGroup
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import android.widget.Toast
import java.util.Locale

class MainActivity : android.app.Activity() {
    private lateinit var urlInput: EditText
    private lateinit var status: TextView
    private val mediaExtensions = Regex("\\.(mp4|m4v|mov|webm|mkv|avi|mp3|m4a|aac|wav)(?:$|\\?)", RegexOption.IGNORE_CASE)

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        buildUi()
        handleIntent(intent)
    }

    override fun onNewIntent(intent: Intent?) {
        super.onNewIntent(intent)
        if (intent != null) handleIntent(intent)
    }

    private fun buildUi() {
        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(24, 28, 24, 24)
            setBackgroundColor(Color.rgb(7, 17, 31))
            layoutDirection = ViewGroup.LAYOUT_DIRECTION_RTL
        }
        val scroll = ScrollView(this).apply { addView(root) }
        setContentView(scroll)

        val title = TextView(this).apply {
            text = "VideoLoad"
            textSize = 28f
            setTextColor(Color.rgb(248, 250, 252))
            gravity = Gravity.RIGHT
        }
        root.addView(title, match())
        val subtitle = TextView(this).apply {
            text = "حفظ الوسائط المسموح بها من روابط مباشرة"
            textSize = 15f
            setTextColor(Color.rgb(167, 243, 208))
            gravity = Gravity.RIGHT
            setPadding(0, 8, 0, 30)
        }
        root.addView(subtitle, match())

        urlInput = EditText(this).apply {
            hint = "ألصق رابط الفيديو هنا"
            hintTextColor = Color.rgb(148, 163, 184)
            setTextColor(Color.WHITE)
            textSize = 15f
            setSingleLine(false)
            minLines = 3
            gravity = Gravity.TOP or Gravity.RIGHT
            setPadding(18, 18, 18, 18)
            setBackgroundColor(Color.rgb(17, 29, 46))
        }
        root.addView(urlInput, LinearLayout.LayoutParams(-1, 150).apply { bottomMargin = 14 })

        val inspect = Button(this).apply {
            text = "فحص الرابط"
            setOnClickListener { inspectAndDownload(false) }
        }
        root.addView(inspect, matchWithMargin(8))
        val download = Button(this).apply {
            text = "تنزيل الملف المسموح"
            setOnClickListener { inspectAndDownload(true) }
        }
        root.addView(download, matchWithMargin(8))

        status = TextView(this).apply {
            textSize = 14f
            setTextColor(Color.rgb(203, 213, 225))
            gravity = Gravity.RIGHT
            setPadding(0, 22, 0, 0)
        }
        root.addView(status, match())
        val note = TextView(this).apply {
            text = "ملاحظة: روابط صفحات YouTube وInstagram وSnapchat ليست ملفات مباشرة. لا يتجاوز التطبيق DRM أو تسجيل الدخول أو حماية المنصات. استخدم المحتوى الذي تملك حق تنزيله فقط."
            textSize = 12f
            setTextColor(Color.rgb(148, 163, 184))
            gravity = Gravity.RIGHT
            setPadding(0, 26, 0, 0)
        }
        root.addView(note, match())
    }

    private fun handleIntent(intent: Intent) {
        val value = when (intent.action) {
            Intent.ACTION_SEND -> intent.getStringExtra(Intent.EXTRA_TEXT)
            Intent.ACTION_VIEW -> intent.dataString
            else -> null
        }
        if (!value.isNullOrBlank()) urlInput.setText(extractUrl(value))
    }

    private fun extractUrl(value: String): String {
        return Regex("https?://[^\\s<>]+", RegexOption.IGNORE_CASE).find(value)?.value
            ?.trimEnd(',', '.', ';', '!', '?', ')') ?: value.trim()
    }

    private fun inspectAndDownload(startDownload: Boolean) {
        val value = extractUrl(urlInput.text.toString())
        urlInput.setText(value)
        val uri = runCatching { Uri.parse(value) }.getOrNull()
        if (uri == null || uri.scheme != "https" || uri.host.isNullOrBlank()) {
            status.text = "أدخل رابطًا صحيحًا يبدأ بـ https://"
            return
        }
        val lower = value.lowercase(Locale.US)
        val isPage = listOf("youtube.", "youtu.be", "instagram.", "snapchat.", "tiktok.", "facebook.", "x.com", "twitter.", "telegram.").any { lower.contains(it) }
        val direct = mediaExtensions.containsMatchIn(uri.path.orEmpty() + if (uri.query.isNullOrBlank()) "" else "?${uri.query}")
        if (isPage && !direct) {
            status.text = "هذا رابط صفحة وليس ملفًا مباشرًا. افتح المصدر الرسمي أو استخدم رابط تنزيل تملك حق استخدامه."
            return
        }
        if (!direct) {
            status.text = "لم أتعرف على امتداد ملف وسائط مباشر. الصيغ المدعومة: MP4, M4V, MOV, WEBM, MKV, AVI, MP3, M4A, AAC, WAV."
            return
        }
        status.text = "الرابط مباشر وجاهز للتنزيل."
        if (startDownload) enqueueDownload(uri)
    }

    private fun enqueueDownload(uri: Uri) {
        val filename = "VideoLoad-${System.currentTimeMillis()}-${uri.lastPathSegment ?: "media.mp4"}"
            .replace(Regex("[^A-Za-z0-9._-]"), "-")
        val request = DownloadManager.Request(uri)
            .setTitle(filename)
            .setDescription("تنزيل من VideoLoad")
            .setNotificationVisibility(DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED)
            .setDestinationInExternalPublicDir(Environment.DIRECTORY_DOWNLOADS, filename)
            .setAllowedOverMetered(true)
            .setAllowedOverRoaming(false)
        val manager = getSystemService(Context.DOWNLOAD_SERVICE) as DownloadManager
        manager.enqueue(request)
        Toast.makeText(this, "بدأ التنزيل. ستجده في مجلد Downloads", Toast.LENGTH_LONG).show()
    }

    private fun match() = LinearLayout.LayoutParams(-1, -2)
    private fun matchWithMargin(top: Int) = LinearLayout.LayoutParams(-1, -2).apply { topMargin = top }
}
