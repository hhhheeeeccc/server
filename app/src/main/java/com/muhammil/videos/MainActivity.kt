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
import kotlinx.coroutines.launch

class MainActivity : AppCompatActivity() {

    private lateinit var urlInput: EditText
    private lateinit var downloadBtn: Button
    private lateinit var statusText: TextView
    private lateinit var progress: ProgressBar
    private lateinit var retryBtn: Button

    private val statusReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context?, intent: Intent?) {
            val text = intent?.getStringExtra(DownloadService.EXTRA_STATUS_TEXT) ?: return
            statusText.text = text
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
        val q = when {
            findViewById<android.widget.RadioButton>(R.id.rbMp3).isChecked -> "mp3"
            findViewById<android.widget.RadioButton>(R.id.rb720).isChecked -> "720"
            else -> "best"
        }
        val service = Intent(this, DownloadService::class.java)
            .putExtra(DownloadService.EXTRA_URL, url)
            .putExtra(DownloadService.EXTRA_QUALITY, q)
        ContextCompat.startForegroundService(this, service)
        statusText.text = getString(R.string.download_started)
    }
}
