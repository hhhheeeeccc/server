package com.muhammil.videos

import com.grack.nanojson.JsonObject
import org.schabi.newpipe.extractor.MediaFormat
import org.schabi.newpipe.extractor.NewPipe
import org.schabi.newpipe.extractor.downloader.Downloader
import org.schabi.newpipe.extractor.downloader.Request
import org.schabi.newpipe.extractor.downloader.Response
import org.schabi.newpipe.extractor.exceptions.ReCaptchaException
import org.schabi.newpipe.extractor.localization.ContentCountry
import org.schabi.newpipe.extractor.localization.Localization
import org.schabi.newpipe.extractor.services.youtube.YoutubeJavaScriptPlayerManager
import org.schabi.newpipe.extractor.services.youtube.YoutubeStreamHelper
import org.schabi.newpipe.extractor.stream.StreamInfo
import java.io.ByteArrayOutputStream
import java.io.IOException
import java.io.InputStream
import java.io.OutputStream
import java.net.HttpURLConnection
import java.net.URL
import java.util.regex.Pattern

/**
 * مستخرج الطبعة الخفيفة — منقول من نسخة DownLite 2.3 المجربة.
 *
 * المسار الأساسي: عميل visionOS — طلب innertube واحد يعيد كل الجودات + الصوت
 * بدون PO Token، وروابط googlevideo تنزل بسرعة كاملة.
 * المسار الاحتياطي: StreamInfo عبر NewPipeExtractor.
 * deThrottle يرفع قيد السرعة (n-param) عبر كود المشغّل بواسطة Rhino.
 */
object YouTubeLite {

    private val VIDEO_ID: Pattern = Pattern.compile(
        "(?i)(?:v=|youtu\\.be/|/shorts/|/embed/|/live/)([A-Za-z0-9_-]{11})"
    )
    private val CLEN: Pattern = Pattern.compile("[?&]clen=(\\d+)")

    @Volatile
    private var inited = false

    private fun init() {
        synchronized(this) {
            if (!inited) {
                NewPipe.init(HttpDownloader())
                inited = true
            }
        }
    }

    /** تهيئة مبكرة عند إقلاع التطبيق (سريعة — شبكة فقط عند أول استخدام). */
    fun warmUp() = init()

    fun clen(url: String?): Long {
        if (url == null) return 0
        val m = CLEN.matcher(url)
        return if (m.find()) runCatching { m.group(1)!!.toLong() }.getOrDefault(0L) else 0L
    }

    /**
     * إعادة كتابة معامل التقييد (n=…) عبر كود مشغّل يوتيوب بواسطة Rhino.
     * بدونه يحدد googlevideo السرعة عند ~80 KB/s. عند الفشل يعيد الرابط الأصلي.
     */
    fun deThrottle(vid: String?, url: String?): String? {
        if (url == null || vid.isNullOrEmpty()) return url
        return try {
            YoutubeJavaScriptPlayerManager.getUrlWithThrottlingParameterDeobfuscated(vid, url)
        } catch (t: Throwable) {
            url
        }
    }

    class LiteStreams(
        val vid: String,
        val title: String,
        val videoUrl: String?, // فيديو فقط (يُدمج مع الصوت) أو muxed جاهز
        val videoHeight: Int,
        val videoSize: Long,
        val audioUrl: String?, // موجود عند الدمج أو عند طلب الصوت وحده
        val audioSize: Long
    )

    /**
     * اختار أفضل تدفق ضمن الارتفاع المطلوب (maxHeight ≤ 0 يعني بلا حد).
     * يعيد null عند الفشل الكامل.
     */
    fun pick(url: String, maxHeight: Int): LiteStreams? {
        val m = VIDEO_ID.matcher(url)
        if (m.find()) {
            visionOs(m.group(1)!!, maxHeight)?.let { return it }
        }
        return streamInfo(url, maxHeight)
    }

    // ------------------------------------------------------------------
    // المسار الأساسي: عميل visionOS
    // ------------------------------------------------------------------

    private fun visionOs(vid: String, maxHeight: Int): LiteStreams? {
        return try {
            val pr = YoutubeStreamHelper.getVisionOsPlayerResponse(
                ContentCountry("US"), Localization("en", "US"), vid, null
            )
            val details = pr.getObject("videoDetails")
            val formats = pr.getObject("streamingData").getArray("adaptiveFormats")
            if (details.isEmpty || formats == null || formats.size() == 0) return null
            val title = details.getString("title", "فيديو")

            // أفضل صوت AAC (m4a)
            var aurl: String? = null
            var abitrate = 0L
            var asize = 0L
            for (i in 0 until formats.size()) {
                val f = formats.getObject(i)
                if (f.getString("mimeType", "").startsWith("audio/mp4")) {
                    val b = f.getLong("bitrate", 0)
                    if (b > abitrate) {
                        abitrate = b
                        aurl = f.getString("url", "")
                        asize = f.getLong("contentLength", clen(aurl))
                    }
                }
            }

            // أفضل فيديو mp4/avc1 ضمن الارتفاع المطلوب (فك ترميز عتادي + دمج MediaMuxer)
            var best: JsonObject? = null
            var bestH = 0
            for (i in 0 until formats.size()) {
                val f = formats.getObject(i)
                val mime = f.getString("mimeType", "")
                if (mime.startsWith("video/mp4") && mime.contains("avc1")) {
                    val h = f.getLong("height", 0).toInt()
                    if (h > bestH && (maxHeight <= 0 || h <= maxHeight)) {
                        bestH = h
                        best = f
                    }
                }
            }
            val vUrl = best?.getString("url", "")
            val vSize = best?.getLong("contentLength", clen(vUrl)) ?: 0L
            LiteStreams(vid, title, vUrl, bestH, vSize, aurl, asize)
        } catch (t: Throwable) {
            null // مشكلة نقل/تحليل — جرّب المسار الاحتياطي
        }
    }

    // ------------------------------------------------------------------
    // المسار الاحتياطي: StreamInfo عبر NewPipeExtractor
    // ------------------------------------------------------------------

    private fun streamInfo(url: String, maxHeight: Int): LiteStreams? {
        return try {
            val info = StreamInfo.getInfo(url)
            val bestA = info.audioStreams
                .filter { it.format == MediaFormat.M4A }
                .maxByOrNull { it.averageBitrate }

            // فيديو فقط (أفضل جودة) مع الصوت للدمج
            val vo = info.videoOnlyStreams
                .filter { maxHeight <= 0 || it.height <= maxHeight }
                .maxByOrNull { it.height }
            if (vo != null && bestA != null) {
                return LiteStreams(
                    info.id, info.name, vo.url, vo.height, clen(vo.url),
                    bestA.url, clen(bestA.url)
                )
            }

            // تدفق muxed جاهز (فيديو+صوت في ملف واحد)
            val muxed = info.videoStreams
                .filter { maxHeight <= 0 || it.height <= maxHeight }
                .maxByOrNull { it.height }
            if (muxed != null) {
                return LiteStreams(
                    info.id, info.name, muxed.url, muxed.height, clen(muxed.url),
                    null, 0L
                )
            }

            // صوت فقط
            if (bestA != null) {
                LiteStreams(
                    info.id, info.name, null, 0, 0L,
                    bestA.url, clen(bestA.url)
                )
            } else null
        } catch (t: Throwable) {
            null
        }
    }

    // ------------------------------------------------------------------
    // طبقة HTTP الخاصة بـ NewPipe
    // ------------------------------------------------------------------

    class HttpDownloader : Downloader() {
        @Throws(IOException::class, ReCaptchaException::class)
        override fun execute(req: Request): Response {
            val c = URL(req.url()).openConnection() as HttpURLConnection
            c.connectTimeout = 10000
            c.readTimeout = 25000
            c.requestMethod = req.httpMethod()
            val hs = req.headers()
            if (hs != null) {
                for ((key, value) in hs.entries) {
                    if (value != null && value.isNotEmpty()) {
                        c.setRequestProperty(key, value[0])
                    }
                }
            }
            val data = req.dataToSend()
            if (data != null) {
                c.doOutput = true
                c.setFixedLengthStreamingMode(data.size)
                val os: OutputStream = c.outputStream
                os.write(data)
                os.close()
            }
            val code = c.responseCode
            val stream: InputStream? = if (code < 400) c.inputStream else c.errorStream
            val ba = ByteArrayOutputStream()
            if (stream != null) {
                val buf = ByteArray(8192)
                var n: Int
                while (stream.read(buf).also { n = it } > 0) {
                    ba.write(buf, 0, n)
                }
                stream.close()
            }
            val body = ba.toString("UTF-8")
            if (code == 429) {
                throw ReCaptchaException("reCaptcha Challenge requested", req.url())
            }
            return Response(code, c.responseMessage, c.headerFields, body, c.url.toString())
        }
    }
}
