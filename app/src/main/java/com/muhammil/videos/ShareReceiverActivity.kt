package com.muhammil.videos

import android.content.Intent
import android.net.Uri
import android.os.Bundle
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import com.google.android.material.dialog.MaterialAlertDialogBuilder

/**
 * تُفتح عند مشاركة رابط من يوتيوب أو إنستغرام (زر المشاركة ← محمّل الفيديوهات).
 * تعرض خيارات الصيغة مباشرة — بدون أي تسجيل دخول ولا شاشات وسيطة.
 */
class ShareReceiverActivity : AppCompatActivity() {

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        val text = intent?.getStringExtra(Intent.EXTRA_TEXT).orEmpty()
        val url = Regex("""https?://\S+""").find(text)?.value

        if (url == null) {
            showErrorDialog(getString(R.string.share_no_url))
            return
        }

        val host = Uri.parse(url).host ?: ""
        val title = when {
            host.contains("instagram.com", true) || host.contains("instagr.am", true) ->
                getString(R.string.share_from_instagram)
            host.contains("youtube.com", true) || host.contains("youtu.be", true) ->
                getString(R.string.share_from_youtube)
            else -> getString(R.string.share_from_link)
        }

        val options = arrayOf(
            getString(R.string.quality_best),
            getString(R.string.quality_720),
            getString(R.string.quality_mp3)
        )
        val qualities = arrayOf("best", "720", "mp3")

        MaterialAlertDialogBuilder(this)
            .setTitle(title)
            .setItems(options) { _, which -> startDownload(url, qualities[which]) }
            .setNegativeButton(R.string.cancel) { _, _ -> finish() }
            .setOnCancelListener { finish() }
            .show()
    }

    private fun startDownload(url: String, quality: String) {
        val service = Intent(this, DownloadService::class.java)
            .putExtra(DownloadService.EXTRA_URL, url)
            .putExtra(DownloadService.EXTRA_QUALITY, quality)
        ContextCompat.startForegroundService(this, service)
        Toast.makeText(this, R.string.download_started, Toast.LENGTH_LONG).show()
        finish()
    }

    private fun showErrorDialog(message: String) {
        MaterialAlertDialogBuilder(this)
            .setTitle(R.string.app_name)
            .setMessage(message)
            .setPositiveButton(R.string.done) { _, _ -> finish() }
            .setOnCancelListener { finish() }
            .show()
    }
}
