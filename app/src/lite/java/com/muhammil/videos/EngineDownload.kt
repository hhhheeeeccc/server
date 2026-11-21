package com.muhammil.videos

import android.media.MediaCodec
import android.media.MediaExtractor
import android.media.MediaMuxer
import android.util.Log
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.File
import java.io.FileInputStream
import java.io.FileOutputStream
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL
import java.nio.ByteBuffer

private const val TAG = "EngineLite"
private const val CHUNK = 4 shl 20 // 4 MB لكل طلب نطاقي — اتصالات جديدة تبقى سريعة
private const val UA_BROWSER =
    "Mozilla/5.0 (Linux; Android 13; Pixel 7) " +
        "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Mobile Safari/537.36"

/**
 * محرك الطبعة الخفيفة: استخراج روابط مباشرة عبر [YouTubeLite]، تنزيل نطاقي سريع،
 * ودمج الفيديو مع الصوت بـ MediaMuxer (مدمج في أندرويد — بدون FFmpeg).
 */
suspend fun engineDownload(
    cacheDir: File,
    url: String,
    quality: String, // best | 720 | mp3
    onProgress: (Float) -> Unit
): EngineOutput = withContext(Dispatchers.IO) {
    try {
        val host = url.lowercase()
        if (host.contains("instagram.com") || host.contains("instagr.am")) {
            throw EngineUnsupportedException("instagram handled by manual strategies")
        }

        val s = YouTubeLite.pick(url, if (quality == "720") 720 else -1)
            ?: throw AppException(R.string.err_youtube)
        Log.i(TAG, "استخراج: ${s.title} | فيديو ${s.videoHeight}p | صوت=${s.audioUrl != null}")

        if (quality == "mp3" || s.videoUrl == null) {
            // صوت فقط (m4a — بدون تحويل لأن الطبعة الخفيفة لا تملك FFmpeg)
            val aUrl = s.audioUrl ?: throw AppException(R.string.err_youtube)
            val f = downloadFile(
                cacheDir,
                YouTubeLite.deThrottle(s.vid, aUrl) ?: aUrl,
                "dl_a_${System.currentTimeMillis()}.m4a",
                0f, 100f, onProgress
            )
            EngineOutput(f, "audio/mp4", s.title)
        } else {
            val vUrl = YouTubeLite.deThrottle(s.vid, s.videoUrl) ?: s.videoUrl
            val aUrl = s.audioUrl

            if (aUrl == null) {
                // تدفق muxed جاهز — لا دمج مطلوب
                val v = downloadFile(
                    cacheDir, vUrl, "dl_v_${System.currentTimeMillis()}.mp4",
                    0f, 100f, onProgress
                )
                EngineOutput(v, "video/mp4", s.title)
            } else {
                // تنزيل الفيديو والصوت ثم دمجهما؛ النسبة موزعة حسب الحجمين
                val total = (s.videoSize + s.audioSize).coerceAtLeast(1L)
                val vSpan = 100f * s.videoSize / total
                val v = downloadFile(
                    cacheDir, vUrl, "dl_v_${System.currentTimeMillis()}.mp4",
                    0f, vSpan, onProgress
                )
                val a = downloadFile(
                    cacheDir, YouTubeLite.deThrottle(s.vid, aUrl) ?: aUrl,
                    "dl_a_${System.currentTimeMillis()}.m4a",
                    vSpan, 100f - vSpan, onProgress
                )
                onProgress(-1f) // الدمج: وضع غير محدد
                val merged = File(cacheDir, "merge_${System.currentTimeMillis()}.mp4")
                try {
                    if (!mergeStreams(v.absolutePath, a.absolutePath, merged)) {
                        Log.e(TAG, "فشل دمج الفيديو مع الصوت")
                        throw AppException(R.string.err_generic)
                    }
                } finally {
                    v.delete()
                    a.delete()
                }
                Log.i(TAG, "تم الدمج: ${merged.length() / 1048576.0} MB")
                EngineOutput(merged, "video/mp4", s.title)
            }
        }
    } catch (e: CancellationException) {
        throw e
    } catch (e: EngineUnsupportedException) {
        throw e
    } catch (e: AppException) {
        throw e
    } catch (e: IOException) {
        Log.e(TAG, "فشل شبكة/تنزيل", e)
        throw AppException(R.string.err_network)
    } catch (e: Exception) {
        Log.e(TAG, "فشل غير متوقع", e)
        throw AppException(R.string.err_youtube)
    }
}

// ------------------------------------------------------------------
// التنزيل النطاقي المتقطع (منقول من DownLite — اتصال جديد كل 4 MB يبقى سريعًا)
// ------------------------------------------------------------------

private fun open(url: String): HttpURLConnection {
    val c = URL(url).openConnection() as HttpURLConnection
    c.connectTimeout = 15000
    c.readTimeout = 60000
    headersFor(c, url)
    return c
}

private fun headersFor(c: HttpURLConnection, url: String) {
    val u = url.lowercase()
    if (u.contains("cdninstagram") || u.contains("fbcdn") || u.contains("instagram.com")) {
        c.setRequestProperty("User-Agent", UA_BROWSER)
        c.setRequestProperty("Referer", "https://www.instagram.com/")
    } else {
        c.setRequestProperty("User-Agent", "MuhammilLite/2.5")
    }
}

private fun downloadFile(
    dir: File,
    url: String,
    fname: String,
    offsetPct: Float,
    spanPct: Float,
    onProgress: (Float) -> Unit
): File {
    val out = File(dir, fname)
    var fo: FileOutputStream? = null
    try {
        fo = FileOutputStream(out)
        val total = YouTubeLite.clen(url)
        var got = 0L
        var lastPct = -1
        if (total > 0) {
            while (got < total) {
                val end = minOf(got + CHUNK, total) - 1
                val c = open(url)
                c.setRequestProperty("Range", "bytes=$got-$end")
                val code = c.responseCode
                if (code != 206 && !(code == 200 && got == 0L)) {
                    c.disconnect()
                    if (code == 416 && got > 0) break // وصلنا النهاية
                    throw IOException("HTTP $code")
                }
                if (got == 0L && c.contentType?.lowercase()?.contains("text/html") == true) {
                    c.disconnect()
                    throw IOException("هذا الرابط صفحة ويب وليس ملفًا")
                }
                c.inputStream.use { input ->
                    val buf = ByteArray(1 shl 16)
                    var n: Int
                    while (input.read(buf).also { n = it } > 0) {
                        fo.write(buf, 0, n)
                        got += n
                        val pct = (got * 100 / total).toInt()
                        if (pct >= lastPct + 2) {
                            lastPct = pct
                            onProgress(offsetPct + spanPct * pct / 100f)
                        }
                    }
                }
                c.disconnect()
                if (code == 200) break // تجاهل الخادم لنطاق Range وأرسل كل شيء
            }
        } else {
            // حجم مجهول: GET واحد والتقدم بالميغابايت (وضع غير محدد)
            val c = open(url)
            if (c.contentType?.lowercase()?.contains("text/html") == true) {
                c.disconnect()
                throw IOException("هذا الرابط صفحة ويب وليس ملفًا")
            }
            c.inputStream.use { input ->
                val buf = ByteArray(1 shl 16)
                var n: Int
                var mb = 0L
                while (input.read(buf).also { n = it } > 0) {
                    fo.write(buf, 0, n)
                    got += n
                    if (got >= ((mb + 4) shl 20).toLong()) {
                        mb = got shr 20
                        onProgress(-1f)
                    }
                }
            }
            c.disconnect()
        }
        fo.close()
        // شبكة أمان: بعض الخوادم تكذب في النوع — لا نحفظ صفحة HTML كوسائط أبدًا
        if (isHtml(out)) {
            out.delete()
            throw IOException("هذا الرابط صفحة ويب وليس ملفًا")
        }
        if (out.length() == 0L) throw IOException("ملف فارغ")
        return out
    } catch (e: Exception) {
        runCatching { fo?.close() }
        out.delete()
        throw e
    }
}

private fun isHtml(f: File): Boolean {
    val len = f.length()
    if (len < 16) return false
    val b = ByteArray(512)
    val n = FileInputStream(f).use { it.read(b) }
    val s = String(b, 0, maxOf(0, n), Charsets.UTF_8).lowercase()
    return s.contains("<!doctype") || s.contains("<html")
}

// ------------------------------------------------------------------
// الدمج بـ MediaMuxer (H.264 + AAC → MP4) — مدمج في أندرويد، بدون FFmpeg
// ------------------------------------------------------------------

private fun pickTrack(ex: MediaExtractor, mimePrefix: String): Int {
    for (i in 0 until ex.trackCount) {
        val mime = ex.getTrackFormat(i).getString(android.media.MediaFormat.KEY_MIME)
        if (mime != null && mime.startsWith(mimePrefix)) return i
    }
    return -1
}

private fun mergeStreams(videoPath: String, audioPath: String, outFile: File): Boolean {
    var ve: MediaExtractor? = null
    var ae: MediaExtractor? = null
    var muxer: MediaMuxer? = null
    try {
        ve = MediaExtractor()
        ve.setDataSource(videoPath)
        val vTrack = pickTrack(ve, "video/")
        if (vTrack < 0) return false
        val vf = ve.getTrackFormat(vTrack)

        ae = MediaExtractor()
        ae.setDataSource(audioPath)
        val aTrack = pickTrack(ae, "audio/")
        if (aTrack < 0) return false
        val af = ae.getTrackFormat(aTrack)

        muxer = MediaMuxer(outFile.absolutePath, MediaMuxer.OutputFormat.MUXER_OUTPUT_MPEG_4)
        val mv = muxer.addTrack(vf)
        val ma = muxer.addTrack(af)
        muxer.start()

        val vMax =
            if (vf.containsKey(android.media.MediaFormat.KEY_MAX_INPUT_SIZE)) vf.getInteger(android.media.MediaFormat.KEY_MAX_INPUT_SIZE) else 1 shl 20
        val aMax =
            if (af.containsKey(android.media.MediaFormat.KEY_MAX_INPUT_SIZE)) af.getInteger(android.media.MediaFormat.KEY_MAX_INPUT_SIZE) else 1 shl 20
        val bb = ByteBuffer.allocate(maxOf(maxOf(vMax, aMax), 1 shl 20))
        val bi = MediaCodec.BufferInfo()

        ve.selectTrack(vTrack)
        while (true) {
            val sz = ve.readSampleData(bb, 0)
            if (sz < 0) break
            bi.offset = 0
            bi.size = sz
            bi.presentationTimeUs = ve.sampleTime
            bi.flags = ve.sampleFlags
            muxer.writeSampleData(mv, bb, bi)
            ve.advance()
        }
        ae.selectTrack(aTrack)
        while (true) {
            val sz = ae.readSampleData(bb, 0)
            if (sz < 0) break
            bi.offset = 0
            bi.size = sz
            bi.presentationTimeUs = ae.sampleTime
            bi.flags = ae.sampleFlags
            muxer.writeSampleData(ma, bb, bi)
            ae.advance()
        }
        return outFile.length() > 0
    } catch (e: Exception) {
        Log.e(TAG, "خطأ دمج", e)
        return false
    } finally {
        runCatching { ve?.release() }
        runCatching { ae?.release() }
        runCatching { muxer?.release() }
    }
}
