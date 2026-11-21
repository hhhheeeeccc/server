package com.muhammil.videos

import android.util.Log
import com.yausername.youtubedl_android.YoutubeDL
import com.yausername.youtubedl_android.YoutubeDLRequest
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.File

private const val TAG = "EngineFull"

/** محرك الطبعة الكاملة: yt-dlp يتولى الاستخراج والدمج وتحويل MP3 داخليًا. */
suspend fun engineDownload(
    cacheDir: File,
    url: String,
    quality: String, // best | 720 | mp3
    onProgress: (Float) -> Unit
): EngineOutput = withContext(Dispatchers.IO) {
    val request = YoutubeDLRequest(url)
    when (quality) {
        "720" -> request.addOption(
            "-f",
            "bv*[ext=mp4][height<=720]+ba[ext=m4a]/b[ext=mp4][height<=720]/b"
        )
        "mp3" -> {
            request.addOption("-x")
            request.addOption("--audio-format", "mp3")
            request.addOption("--audio-quality", "0")
        }
        else -> request.addOption("-f", "bv*[ext=mp4]+ba[ext=m4a]/b[ext=mp4]/b")
    }
    if (quality != "mp3") request.addOption("--merge-output-format", "mp4")
    request.addOption("--no-playlist")
    request.addOption("-P", cacheDir.absolutePath)
    request.addOption("-o", "%(title).80s.%(ext)s")

    val processId = "dl-${System.currentTimeMillis()}"
    try {
        YoutubeDL.getInstance().execute(
            request,
            processId,
            callback = { progress: Float, _eta: Long, _line: String ->
                onProgress(progress)
            }
        )
    } finally {
        runCatching { YoutubeDL.getInstance().destroyProcessById(processId) }
    }

    val out = cacheDir.listFiles()
        ?.filter { it.isFile && !it.name.endsWith(".part") }
        ?.maxByOrNull { it.lastModified() }
        ?: throw AppException(R.string.err_generic)

    val mime = if (out.extension.equals("mp3", true)) "audio/mpeg" else "video/mp4"
    Log.i(TAG, "تم التنزيل: ${out.name} (${mime})")
    EngineOutput(out, mime, out.nameWithoutExtension)
}
