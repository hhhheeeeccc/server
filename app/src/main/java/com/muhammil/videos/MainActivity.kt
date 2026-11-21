package com.muhammil.videos

import android.Manifest
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.os.Build
import android.os.Bundle
import android.view.View
import android.widget.Button
import android.widget.EditText
import android.widget.ProgressBar
import android.widget.TextView
import android.widget.Toast
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import androidx.core.content.PermissionChecker
import androidx.lifecycle.lifecycleScope
import com.google.android.material.card.MaterialCardView
import kotlinx.coroutines.launch

class MainActivity : AppCompatActivity() {

    private lateinit var urlInput: EditText
    private lateinit var downloadBtn: Button
    private lateinit var statusText: TextView
    private lateinit var progress: ProgressBar
    private lateinit var retryBtn: Button
    private var selectedQuality = "best"

    private val statusReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context?, intent: Intent?) {
            val text = intent?.getStringExtra(DownloadService.EXTRA_STATUS_TEXT) ?: return
            statusText.text = text
            val p = intent.getFloatExtra(DownloadService.EXTRA_STATUS_PROGRESS, -1f)
            if (p >= 0) {
                progress.isIndeterminate = false
                progress.progress = p.toInt()
            } else {
                progress.isIndeterminate = true
            }
        }
    }

    private val notifPermissionLauncher =
        registerForActivityResult(ActivityResultContracts.RequestPermission()) { }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)

        urlInput = findViewById(R.id.urlInput)
        downloadBtn = findViewById(R.id.downloadBtn)
        statusText = findViewById(R.id.statusText)
        progress = findViewById(R.id.progress)
        retryBtn = findViewById(R.id.retryBtn)

        findViewById<Button>(R.id.pasteBtn).setOnClickListener { pasteFromClipboard() }
        downloadBtn.setOnClickListener { startDownload() }
        retryBtn.setOnClickListener {
            Engine.retry(this)
            retryBtn.visibility = View.GONE
        }

        // بطاقات الصيغ القابلة للاختيار
        applyQualitySelection()
        listOf(
            "best" to R.id.optBest,
            "720" to R.id.opt720,
            "mp3" to R.id.optMp3
        ).forEach { (key, cardId) ->
            findViewById<MaterialCardView>(cardId).setOnClickListener {
                selectedQuality = key
                applyQualitySelection()
            }
        }

        askForNotificationPermission()

        ContextCompat.registerReceiver(
            this, statusReceiver,
            IntentFilter(DownloadService.ACTION_STATUS),
            ContextCompat.RECEIVER_NOT_EXPORTED
        )

        // حالة المحرك مرئية دائمًا — لا فشل صامت
        lifecycleScope.launch {
            Engine.state.collect { state -> render(state) }
        }
    }

    override fun onDestroy() {
        unregisterReceiver(statusReceiver)
        super.onDestroy()
    }

    private fun render(state: Engine.State) {
        when (state) {
            is Engine.State.NotStarted, is Engine.State.Initializing -> {
                progress.isIndeterminate = true
                progress.visibility = View.VISIBLE
                statusText.text = getString(R.string.engine_preparing)
                downloadBtn.isEnabled = false
                retryBtn.visibility = View.GONE
            }
            is Engine.State.Ready -> {
                progress.visibility = View.GONE
                statusText.text = getString(R.string.engine_ready)
                downloadBtn.isEnabled = true
                retryBtn.visibility = View.GONE
            }
            is Engine.State.Failed -> {
                progress.visibility = View.GONE
                statusText.text = getString(R.string.engine_failed)
                downloadBtn.isEnabled = false
                retryBtn.visibility = View.VISIBLE
            }
        }
    }

    /** تحديث مظهر بطاقات الصيغ حسب الاختيار الحالي. */
    private fun applyQualitySelection() {
        listOf(
            "best" to (R.id.optBest to R.id.checkBest),
            "720" to (R.id.opt720 to R.id.check720),
            "mp3" to (R.id.optMp3 to R.id.checkMp3)
        ).forEach { (key, ids) ->
            val (cardId, checkId) = ids
            val selected = key == selectedQuality
            val card = findViewById<MaterialCardView>(cardId)
            card.strokeWidth = if (selected) dp(2) else 0
            if (selected) {
                card.strokeColor = ContextCompat.getColor(this, R.color.primary)
                card.setCardBackgroundColor(ContextCompat.getColor(this, R.color.primary_container))
            } else {
                card.setCardBackgroundColor(ContextCompat.getColor(this, R.color.surface_lighter))
            }
            findViewById<View>(checkId).visibility = if (selected) View.VISIBLE else View.GONE
        }
    }

    private fun dp(value: Int): Int = (value * resources.displayMetrics.density).toInt()

    private fun pasteFromClipboard() {
        val cm = getSystemService(Context.CLIPBOARD_SERVICE) as android.content.ClipboardManager
        val clip = cm.primaryClip
        if (clip != null && clip.itemCount > 0) {
            val text = clip.getItemAt(0).coerceToText(this)?.toString().orEmpty()
            if (text.isNotBlank()) urlInput.setText(text.trim())
        }
    }

    private fun askForNotificationPermission() {
        if (Build.VERSION.SDK_INT >= 33) {
            val granted = PermissionChecker.checkSelfPermission(
                this, Manifest.permission.POST_NOTIFICATIONS
            ) == PermissionChecker.PERMISSION_GRANTED
            if (!granted) {
                notifPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
            }
        }
    }

    private fun startDownload() {
        val url = urlInput.text.toString().trim()
        if (!android.util.Patterns.WEB_URL.matcher(url).matches() || !url.contains("http")) {
            Toast.makeText(this, R.string.invalid_url, Toast.LENGTH_LONG).show()
            return
        }
        if (Engine.state.value !is Engine.State.Ready) {
            Toast.makeText(this, R.string.engine_preparing, Toast.LENGTH_SHORT).show()
            return
        }
        val q = selectedQuality
        val service = Intent(this, DownloadService::class.java)
            .putExtra(DownloadService.EXTRA_URL, url)
            .putExtra(DownloadService.EXTRA_QUALITY, q)
        ContextCompat.startForegroundService(this, service)
        statusText.text = getString(R.string.download_started)
    }
}
