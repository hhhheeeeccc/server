package com.videoload.android

import android.app.Activity
import android.app.AlertDialog
import android.app.DownloadManager
import android.content.Context
import android.content.Intent
import android.graphics.Color
import android.graphics.Typeface
import android.graphics.drawable.GradientDrawable
import android.net.Uri
import android.os.Bundle
import android.os.Environment
import android.provider.DocumentsContract
import android.view.Gravity
import android.view.View
import android.view.ViewGroup
import android.view.inputmethod.InputMethodManager
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.ProgressBar
import android.widget.ScrollView
import android.widget.TextView
import android.widget.Toast
import java.net.URLDecoder
import java.nio.charset.StandardCharsets
import java.util.Locale

class MainActivity : Activity() {
    private lateinit var urlInput: EditText
    private lateinit var status: TextView
    private lateinit var downloadsList: LinearLayout
    private lateinit var homePage: LinearLayout
    private lateinit var downloadsPage: LinearLayout
    private lateinit var homeTab: TextView
    private lateinit var downloadsTab: TextView
    private lateinit var downloadManager: DownloadManager
    private val preferences by lazy { getSharedPreferences("nazzel_video", Context.MODE_PRIVATE) }
    private var pendingDownloadUri: Uri? = null

    private val mediaExtensions = setOf("mp4", "m4v", "mov", "webm", "mkv", "avi")
    private val platformDomains = listOf(
        "youtube.com", "youtu.be", "youtube-nocookie.com", "instagram.com",
        "snapchat.com", "tiktok.com", "facebook.com", "fb.watch", "x.com",
        "twitter.com", "t.me", "telegram.org", "telegram.me",
    )

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        downloadManager = getSystemService(Context.DOWNLOAD_SERVICE) as DownloadManager
        buildUi()
        handleIntent(intent)
        renderDownloads()
    }

    override fun onResume() {
        super.onResume()
        if (::downloadsList.isInitialized) renderDownloads()
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        handleIntent(intent)
        showPage(false)
    }

    override fun onRequestPermissionsResult(requestCode: Int, permissions: Array<out String>, grantResults: IntArray) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        if (requestCode == NOTIFICATION_PERMISSION_REQUEST) {
            val uri = pendingDownloadUri
            pendingDownloadUri = null
            if (uri != null) enqueueDownload(uri)
        }
    }

    private fun buildUi() {
        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(dp(20), dp(24), dp(20), dp(24))
            setBackgroundColor(Color.rgb(7, 17, 31))
            layoutDirection = ViewGroup.LAYOUT_DIRECTION_RTL
        }
        val scroll = ScrollView(this).apply { isFillViewport = true; addView(root) }
        setContentView(scroll)

        val header = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = Gravity.CENTER_VERTICAL
            layoutDirection = ViewGroup.LAYOUT_DIRECTION_RTL
        }
        val logo = text("↓", 25, Color.rgb(7, 17, 31), true).apply {
            gravity = Gravity.CENTER
            background = rounded(Color.rgb(167, 243, 208), 16)
        }
        header.addView(logo, LinearLayout.LayoutParams(dp(48), dp(48)).apply { marginEnd = dp(12) })
        val brand = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL; gravity = Gravity.START }
        brand.addView(text("نزّل الفيديو", 21, Color.WHITE, true))
        brand.addView(text("حفظ واضح. بدون تجاوز.", 12, Color.rgb(148, 163, 184)))
        header.addView(brand)
        root.addView(header)

        val tabs = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            layoutDirection = ViewGroup.LAYOUT_DIRECTION_RTL
            background = rounded(Color.rgb(17, 29, 46), 16)
            setPadding(dp(4), dp(4), dp(4), dp(4))
        }
        homeTab = tab("الرئيسية", true).also { it.setOnClickListener { showPage(false) } }
        downloadsTab = tab("التنزيلات", false).also { it.setOnClickListener { renderDownloads(); showPage(true) } }
        tabs.addView(homeTab, LinearLayout.LayoutParams(0, dp(44), 1f))
        tabs.addView(downloadsTab, LinearLayout.LayoutParams(0, dp(44), 1f))
        root.addView(tabs, LinearLayout.LayoutParams(-1, -2).apply { topMargin = dp(22); bottomMargin = dp(20) })

        homePage = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }
        homePage.addView(text("نزّل ما تملك حق حفظه", 26, Color.WHITE, true))
        homePage.addView(text(
            "الصق رابط ملف فيديو مباشر أو شاركه إلى التطبيق. روابط صفحات المنصات تفتح في تطبيقها الرسمي ولا نستخرج الفيديو منها.",
            14, Color.rgb(148, 163, 184),
        ).apply { setPadding(0, dp(8), 0, dp(20)) })

        val inputCard = card().apply { orientation = LinearLayout.VERTICAL }
        inputCard.addView(text("رابط الفيديو", 14, Color.rgb(226, 232, 240), true))
        urlInput = EditText(this).apply {
            hint = "https://.../video.mp4"
            setHintTextColor(Color.rgb(100, 116, 139))
            setTextColor(Color.WHITE)
            textSize = 14f
            layoutDirection = ViewGroup.LAYOUT_DIRECTION_LTR
            textDirection = View.TEXT_DIRECTION_LTR
            gravity = Gravity.START or Gravity.TOP
            minLines = 3
            setPadding(dp(13), dp(13), dp(13), dp(13))
            background = rounded(Color.rgb(7, 17, 31), 13, Color.rgb(38, 59, 83))
            inputType = android.text.InputType.TYPE_CLASS_TEXT or android.text.InputType.TYPE_TEXT_VARIATION_URI
        }
        inputCard.addView(urlInput, LinearLayout.LayoutParams(-1, dp(96)).apply { topMargin = dp(12) })
        val actions = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            layoutDirection = ViewGroup.LAYOUT_DIRECTION_RTL
        }
        actions.addView(actionButton("فحص الرابط", true) { inspectAndDownload(false) },
            LinearLayout.LayoutParams(0, dp(48), 1f).apply { marginEnd = dp(8) })
        actions.addView(actionButton("بدء التنزيل", false) { inspectAndDownload(true) },
            LinearLayout.LayoutParams(0, dp(48), 1f))
        inputCard.addView(actions, LinearLayout.LayoutParams(-1, -2).apply { topMargin = dp(12) })
        homePage.addView(inputCard)

        status = text("شارك رابطًا أو الصقه هنا لبدء الفحص.", 14, Color.rgb(203, 213, 225)).apply {
            setPadding(dp(14), dp(14), dp(14), dp(14))
            background = rounded(Color.rgb(17, 29, 46), 15)
        }
        homePage.addView(status, LinearLayout.LayoutParams(-1, -2).apply { topMargin = dp(14) })
        homePage.addView(text(
            "لا يمكن للتطبيق استخدام جلسة دخول تطبيق آخر أو تجاوز DRM وتسجيل الدخول وحماية المنصات. استخدم روابط مباشرة ومصرحًا بتنزيلها فقط.",
            12, Color.rgb(100, 116, 139),
        ).apply { setPadding(dp(4), dp(18), dp(4), dp(10)) })
        root.addView(homePage)

        downloadsPage = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL; visibility = View.GONE }
        downloadsPage.addView(text("مكتبتك", 25, Color.WHITE, true))
        downloadsPage.addView(text("التنزيلات التي بدأتها من هذا التطبيق على هذا الجهاز.", 14, Color.rgb(148, 163, 184))
            .apply { setPadding(0, dp(6), 0, dp(18)) })
        downloadsList = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }
        downloadsPage.addView(downloadsList)
        root.addView(downloadsPage)
    }

    private fun showPage(showDownloads: Boolean) {
        homePage.visibility = if (showDownloads) View.GONE else View.VISIBLE
        downloadsPage.visibility = if (showDownloads) View.VISIBLE else View.GONE
        homeTab.background = rounded(if (showDownloads) Color.TRANSPARENT else Color.rgb(167, 243, 208), 12)
        homeTab.setTextColor(if (showDownloads) Color.rgb(203, 213, 225) else Color.rgb(7, 17, 31))
        downloadsTab.background = rounded(if (showDownloads) Color.rgb(167, 243, 208) else Color.TRANSPARENT, 12)
        downloadsTab.setTextColor(if (showDownloads) Color.rgb(7, 17, 31) else Color.rgb(203, 213, 225))
    }

    private fun handleIntent(intent: Intent?) {
        val shared = when (intent?.action) {
            Intent.ACTION_SEND -> intent.getStringExtra(Intent.EXTRA_TEXT)
            Intent.ACTION_VIEW -> intent.dataString
            else -> null
        }
        if (!shared.isNullOrBlank()) {
            urlInput.setText(extractUrl(shared))
            status.text = "وصل الرابط من المشاركة. اضغط فحص الرابط لمعرفة ما إذا كان ملفًا مباشرًا."
        }
    }

    private fun extractUrl(value: String): String {
        return Regex("https?://[^\\s<>]+", RegexOption.IGNORE_CASE).find(value)?.value
            ?.trimEnd(',', '.', ';', '!', '?', ')') ?: value.trim()
    }

    private fun inspectAndDownload(startDownload: Boolean) {
        val raw = extractUrl(urlInput.text.toString())
        urlInput.setText(raw)
        val uri = runCatching { Uri.parse(raw) }.getOrNull()
        val host = uri?.host?.lowercase(Locale.ROOT)?.removePrefix("www.")
        if (uri == null || uri.scheme != "https" || host.isNullOrBlank() || !uri.encodedUserInfo.isNullOrEmpty()) {
            status.text = "أدخل رابط HTTPS صحيحًا من دون اسم مستخدم أو كلمة مرور داخل الرابط."
            return
        }
        if (isPlatformHost(host)) {
            status.text = "هذا رابط صفحة منصة وليس ملف فيديو مباشرًا. افتحه في التطبيق الرسمي أو استخدم رابط ملف تملك حق تنزيله."
            return
        }
        val extension = uri.path.orEmpty().substringAfterLast('.', "").lowercase(Locale.ROOT)
        if (extension !in mediaExtensions) {
            status.text = "الرابط لا يشير إلى ملف فيديو مباشر مدعوم (MP4 أو M4V أو MOV أو WEBM أو MKV أو AVI)."
            return
        }
        status.text = "يبدو أن الرابط يشير إلى ملف مباشر ويستخدم HTTPS. تأكد من امتلاكك حق تنزيله."
        if (startDownload) {
            AlertDialog.Builder(this)
                .setTitle("تأكيد التنزيل")
                .setMessage("أؤكد أن لدي حق استخدام هذا الفيديو وأن تنزيله مسموح به.")
                .setNegativeButton("إلغاء", null)
                .setPositiveButton("أوافق وأبدأ") { _, _ -> requestNotificationThenDownload(uri) }
                .show()
        }
    }

    private fun requestNotificationThenDownload(uri: Uri) {
        if (android.os.Build.VERSION.SDK_INT >= 33 &&
            checkSelfPermission(android.Manifest.permission.POST_NOTIFICATIONS) != android.content.pm.PackageManager.PERMISSION_GRANTED
        ) {
            pendingDownloadUri = uri
            requestPermissions(arrayOf(android.Manifest.permission.POST_NOTIFICATIONS), NOTIFICATION_PERMISSION_REQUEST)
        } else {
            enqueueDownload(uri)
        }
    }

    private fun isPlatformHost(host: String): Boolean = platformDomains.any { domain ->
        host == domain || host.endsWith(".$domain")
    }

    private fun enqueueDownload(uri: Uri) {
        val rawName = uri.lastPathSegment?.let {
            runCatching { URLDecoder.decode(it, StandardCharsets.UTF_8.name()) }.getOrDefault(it)
        }.orEmpty().ifBlank { "video.mp4" }
        val safeName = rawName.replace(Regex("[^A-Za-z0-9._-]"), "-").trim('-', '.')
            .ifBlank { "video.mp4" }.take(120)
        val request = DownloadManager.Request(uri)
            .setTitle(safeName)
            .setDescription("تنزيل بواسطة نزّل الفيديو")
            .setNotificationVisibility(DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED)
            .setDestinationInExternalPublicDir(Environment.DIRECTORY_DOWNLOADS, "نزّل الفيديو/$safeName")
            .setAllowedOverMetered(true)
            .setAllowedOverRoaming(false)
        try {
            val id = downloadManager.enqueue(request)
            val ids = preferences.getStringSet(KEY_DOWNLOAD_IDS, emptySet())?.toMutableSet() ?: mutableSetOf()
            ids.add(id.toString())
            preferences.edit().putStringSet(KEY_DOWNLOAD_IDS, ids).apply()
            status.text = "بدأ التنزيل. ستجد تقدمه في تبويب التنزيلات، والملف في Downloads/نزّل الفيديو."
            Toast.makeText(this, "بدأ التنزيل", Toast.LENGTH_SHORT).show()
            renderDownloads()
        } catch (error: Exception) {
            status.text = "تعذر بدء التنزيل: ${error.localizedMessage ?: "تحقق من الاتصال وحاول مجددًا."}"
        }
    }

    private fun renderDownloads() {
        if (!::downloadsList.isInitialized) return
        downloadsList.removeAllViews()
        val ids = preferences.getStringSet(KEY_DOWNLOAD_IDS, emptySet())
            .orEmpty().mapNotNull { it.toLongOrNull() }.sortedDescending()
        if (ids.isEmpty()) {
            downloadsList.addView(card().apply {
                addView(text("لا توجد تنزيلات بعد", 16, Color.WHITE, true))
                addView(text("ابدأ برابط فيديو مباشر مصرح به.", 13, Color.rgb(148, 163, 184))
                    .apply { setPadding(0, dp(6), 0, 0) })
            })
            return
        }
        val validIds = mutableSetOf<String>()
        for (id in ids) {
            val cursor = downloadManager.query(DownloadManager.Query().setFilterById(id)) ?: continue
            cursor.use {
                if (!it.moveToFirst()) return@use
                validIds.add(id.toString())
                val title = preferences.getString(RENAMED_PREFIX + id, null)
                    ?: it.stringColumn(DownloadManager.COLUMN_TITLE) ?: "فيديو"
                val state = it.getInt(it.getColumnIndexOrThrow(DownloadManager.COLUMN_STATUS))
                val downloaded = it.getLong(it.getColumnIndexOrThrow(DownloadManager.COLUMN_BYTES_DOWNLOADED_SO_FAR))
                val total = it.getLong(it.getColumnIndexOrThrow(DownloadManager.COLUMN_TOTAL_SIZE_BYTES))
                val stateText = when (state) {
                    DownloadManager.STATUS_PENDING -> "في قائمة الانتظار"
                    DownloadManager.STATUS_RUNNING -> if (total > 0) "جارٍ التنزيل — ${((downloaded * 100) / total).coerceIn(0, 100)}٪" else "جارٍ التنزيل"
                    DownloadManager.STATUS_PAUSED -> "متوقف مؤقتًا"
                    DownloadManager.STATUS_SUCCESSFUL -> "اكتمل التنزيل${if (total > 0) " — ${formatSize(total)}" else ""}"
                    DownloadManager.STATUS_FAILED -> "تعذر التنزيل (رمز ${it.getInt(it.getColumnIndexOrThrow(DownloadManager.COLUMN_REASON))})"
                    else -> "حالة غير معروفة"
                }
                val itemCard = card().apply { orientation = LinearLayout.VERTICAL }
                itemCard.addView(text(title, 15, Color.WHITE, true))
                itemCard.addView(text(stateText, 12, if (state == DownloadManager.STATUS_FAILED) Color.rgb(251, 113, 133) else Color.rgb(167, 243, 208))
                    .apply { setPadding(0, dp(6), 0, dp(8)) })
                if (state == DownloadManager.STATUS_RUNNING || state == DownloadManager.STATUS_PAUSED) {
                    val progress = ProgressBar(this, null, android.R.attr.progressBarStyleHorizontal).apply {
                        max = 100
                        this.progress = if (total > 0) ((downloaded * 100) / total).toInt().coerceIn(0, 100) else 0
                    }
                    itemCard.addView(progress, LinearLayout.LayoutParams(-1, dp(6)).apply { bottomMargin = dp(10) })
                }
                val controls = LinearLayout(this).apply { orientation = LinearLayout.HORIZONTAL; layoutDirection = ViewGroup.LAYOUT_DIRECTION_RTL }
                val canRename = state == DownloadManager.STATUS_SUCCESSFUL && downloadManager.getUriForDownloadedFile(id) != null
                controls.addView(actionButton("تعديل الاسم", false, enabled = canRename) { renameDownload(id, title) },
                    LinearLayout.LayoutParams(0, dp(42), 1f).apply { marginEnd = dp(8) })
                controls.addView(actionButton(if (state == DownloadManager.STATUS_SUCCESSFUL) "حذف الملف" else "إلغاء", false) {
                    confirmRemove(id, title)
                }, LinearLayout.LayoutParams(0, dp(42), 1f))
                itemCard.addView(controls)
                downloadsList.addView(itemCard, LinearLayout.LayoutParams(-1, -2).apply { bottomMargin = dp(12) })
            }
        }
        if (validIds.size != ids.size) {
            preferences.edit().putStringSet(KEY_DOWNLOAD_IDS, validIds).apply()
        }
    }

    private fun renameDownload(id: Long, currentTitle: String) {
        val extension = currentTitle.substringAfterLast('.', "").takeIf { it.isNotBlank() }?.let { ".$it" }.orEmpty()
        val baseName = if (extension.isNotEmpty()) currentTitle.removeSuffix(extension) else currentTitle
        val input = EditText(this).apply { setText(baseName); selectAll() }
        AlertDialog.Builder(this)
            .setTitle("تعديل اسم الفيديو")
            .setMessage("سيُحفظ امتداد الملف تلقائيًا.")
            .setView(input)
            .setNegativeButton("إلغاء", null)
            .setPositiveButton("حفظ") { _, _ ->
                val edited = input.text.toString().trim().replace(Regex("[/\\\\:*?\"<>|]"), "-").take(100)
                if (edited.isBlank()) {
                    Toast.makeText(this, "أدخل اسمًا صالحًا", Toast.LENGTH_SHORT).show()
                    return@setPositiveButton
                }
                val newName = if (extension.isNotEmpty() && !edited.endsWith(extension, ignoreCase = true)) edited + extension else edited
                val fileUri = downloadManager.getUriForDownloadedFile(id)
                val renamed = runCatching { fileUri?.let { DocumentsContract.renameDocument(contentResolver, it, newName) } }.getOrNull()
                if (renamed != null) {
                    preferences.edit().putString(RENAMED_PREFIX + id, newName).apply()
                    Toast.makeText(this, "تم تعديل الاسم", Toast.LENGTH_SHORT).show()
                    renderDownloads()
                } else {
                    Toast.makeText(this, "تعذر تعديل الاسم من مزود التنزيلات على هذا الجهاز", Toast.LENGTH_LONG).show()
                }
            }
            .show()
    }

    private fun confirmRemove(id: Long, title: String) {
        AlertDialog.Builder(this)
            .setTitle("حذف التنزيل؟")
            .setMessage("سيُحذف الملف \"$title\" من مجلد Downloads.")
            .setNegativeButton("إلغاء", null)
            .setPositiveButton("حذف") { _, _ ->
                downloadManager.remove(id)
                val ids = preferences.getStringSet(KEY_DOWNLOAD_IDS, emptySet())?.toMutableSet() ?: mutableSetOf()
                ids.remove(id.toString())
                preferences.edit().putStringSet(KEY_DOWNLOAD_IDS, ids).remove(RENAMED_PREFIX + id).apply()
                renderDownloads()
            }
            .show()
    }

    private fun formatSize(size: Long): String = when {
        size < 1024 -> "$size بايت"
        size < 1024 * 1024 -> String.format(Locale.US, "%.1f كيلوبايت", size / 1024.0)
        else -> String.format(Locale.US, "%.1f ميغابايت", size / (1024.0 * 1024.0))
    }

    private fun android.database.Cursor.stringColumn(name: String): String? {
        val index = getColumnIndex(name)
        return if (index >= 0 && !isNull(index)) getString(index) else null
    }

    private fun card(): LinearLayout = LinearLayout(this).apply {
        orientation = LinearLayout.VERTICAL
        setPadding(dp(16), dp(16), dp(16), dp(16))
        background = rounded(Color.rgb(17, 29, 46), 18, Color.rgb(34, 51, 74))
    }

    private fun text(value: String, size: Int, color: Int, bold: Boolean = false) = TextView(this).apply {
        text = value
        textSize = size.toFloat()
        setTextColor(color)
        gravity = Gravity.START
        textDirection = View.TEXT_DIRECTION_RTL
        if (bold) typeface = Typeface.DEFAULT_BOLD
    }

    private fun tab(label: String, selected: Boolean) = TextView(this).apply {
        text = label
        textSize = 14f
        gravity = Gravity.CENTER
        setTypeface(null, Typeface.BOLD)
        background = rounded(if (selected) Color.rgb(167, 243, 208) else Color.TRANSPARENT, 12)
        setTextColor(if (selected) Color.rgb(7, 17, 31) else Color.rgb(203, 213, 225))
    }

    private fun actionButton(label: String, primary: Boolean, enabled: Boolean = true, action: () -> Unit) = Button(this).apply {
        text = label
        isAllCaps = false
        textSize = 13f
        isEnabled = enabled
        setTextColor(if (primary) Color.rgb(7, 17, 31) else Color.rgb(226, 232, 240))
        background = rounded(if (primary) Color.rgb(167, 243, 208) else Color.rgb(30, 41, 59), 13)
        setPadding(dp(8), 0, dp(8), 0)
        setOnClickListener {
            (getSystemService(Context.INPUT_METHOD_SERVICE) as InputMethodManager)
                .hideSoftInputFromWindow(windowToken, 0)
            action()
        }
    }

    private fun rounded(color: Int, radius: Int, stroke: Int? = null) = GradientDrawable().apply {
        setColor(color)
        cornerRadius = dp(radius).toFloat()
        if (stroke != null) setStroke(dp(1), stroke)
    }

    private fun dp(value: Int): Int = (value * resources.displayMetrics.density).toInt()

    private companion object {
        const val KEY_DOWNLOAD_IDS = "download_ids"
        const val RENAMED_PREFIX = "renamed_"
        const val NOTIFICATION_PERMISSION_REQUEST = 101
    }
}
