package com.muhammil.videos

import android.util.Log
import okhttp3.OkHttpClient
import okhttp3.Request
import java.util.concurrent.TimeUnit

/**
 * مستخرج إنستغرام بدون أي تسجيل دخول.
 *
 * لا يحتوي هذا الملف — ولا التطبيق كله — على أي شاشة أو طلب لتسجيل الدخول إلى إنستغرام.
 * نستخرج الفيديو من المنشورات العامة عبر ثلاث استراتيجيات متتالية:
 *
 *  1) graphqlPost : استعلام واجهة Instagram GraphQL العامة (doc_id) بدون حساب.
 *  2) fromXdt     : قراءة بيانات xdt المضمّنة في صفحة المنشور نفسها.
 *  3) viaEmbed    : صفحة الـ embed الرسمية للمنشور.
 *
 * ملاحظة صادقة للمستخدم: أندرويد يمنع أي تطبيق من قراءة جلسة تطبيق إنستغرام المثبَّت
 * على الهاتف (عزل النظام)، لذلك لا يمكن "معرفة" حسابك مباشرة. المحتوى العام يُنزَّل
 * بدون أي حساب، أما المحتوى الخاص/الستوري فلا يمكن الوصول إليه، ونعرض له رسالة عربية واضحة.
 */
object InstagramExtractor {

    private const val TAG = "InstagramExtractor"

    private const val IG_APP_ID = "936619743392459"
    private const val FB_LSD = "AVqbxe3J_YA"
    private const val ASBD_ID = "129477"
    private const val DOC_ID = "8845758582119845" // PolarisPostActionLoadPostQueryQuery (عام)

    const val UA =
        "Mozilla/5.0 (Linux; Android 13; Pixel 7) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36"

    private val client: OkHttpClient = OkHttpClient.Builder()
        .connectTimeout(20, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS)
        .followRedirects(true)
        .followSslRedirects(true)
        .build()

    sealed class IgResult {
        data class Video(val videoUrl: String, val title: String) : IgResult()
        data object PrivateOrRestricted : IgResult()
        data object NotFound : IgResult()
        data class NetworkError(val message: String) : IgResult()
    }

    /** نقطة الدخول: يجرّب الاستراتيجيات بالترتيب. */
    fun extract(postUrl: String): IgResult {
        val code = resolveShortcode(postUrl)
            ?: return IgResult.NotFound
        Log.i(TAG, "shortcode=$code")

        graphqlPost(code)?.let { return it }
        Log.i(TAG, "graphqlPost فشلت، نجرب fromXdt")
        fromXdt(code)?.let { return it }
        Log.i(TAG, "fromXdt فشلت، نجرب viaEmbed")
        viaEmbed(code)?.let { return it }

        // فشلت كل الاستراتيجيات: الأغلب أن المحتوى خاص أو محجوب عن الزوار
        return IgResult.PrivateOrRestricted
    }

    // ------------------------------------------------------------------
    // استخراج الرمز المختصر (shortcode) من الروابط المختلفة
    // ------------------------------------------------------------------

    private val shortcodeRegex =
        Regex("""instagram\.com/(?:[A-Za-z0-9_.]+/)?(?:p|reel|reels|tv)/([A-Za-z0-9_-]+)""")

    private fun resolveShortcode(url: String): String? {
        shortcodeRegex.find(url)?.let { return it.groupValues[1] }
        // روابط المشاركة المختصرة مثل instagram.com/share/xxx تُحوَّل إلى الرابط الكامل
        if (url.contains("instagram.com", ignoreCase = true) ||
            url.contains("instagr.am", ignoreCase = true)
        ) {
            try {
                val finalUrl = followRedirects(url)
                shortcodeRegex.find(finalUrl)?.let { return it.groupValues[1] }
            } catch (e: Exception) {
                Log.w(TAG, "تعذر تتبع تحويل الرابط", e)
            }
        }
        return null
    }

    private fun followRedirects(url: String): String {
        val request = Request.Builder()
            .url(url)
            .header("User-Agent", UA)
            .get()
            .build()
        client.newCall(request).execute().use { resp ->
            return resp.request.url.toString()
        }
    }

    // ------------------------------------------------------------------
    // الاستراتيجية 1: graphqlPost — استعلام GraphQL العام بدون حساب
    // ------------------------------------------------------------------

    private fun graphqlPost(code: String): IgResult.Video? {
        return try {
            val variables =
                """{"shortcode":"$code","fetch_tagged_user_count":null,"hoisted_comment_id":null,"hoisted_reply_id":null}"""
            val body = okhttp3.FormBody.Builder()
                .add("doc_id", DOC_ID)
                .add("variables", variables)
                .add("lsd", FB_LSD)
                .build()
            val request = Request.Builder()
                .url("https://www.instagram.com/api/graphql")
                .post(body)
                .header("User-Agent", UA)
                .header("Content-Type", "application/x-www-form-urlencoded")
                .header("X-IG-App-ID", IG_APP_ID)
                .header("X-FB-LSD", FB_LSD)
                .header("X-ASBD-ID", ASBD_ID)
                .header("Referer", "https://www.instagram.com/")
                .header("Origin", "https://www.instagram.com")
                .build()

            client.newCall(request).execute().use { resp ->
                val json = resp.body?.string() ?: return null
                if (!resp.isSuccessful) {
                    Log.w(TAG, "graphqlPost http=${resp.code}")
                    return null
                }
                parseXdtJson(json, code)
            }
        } catch (e: Exception) {
            Log.w(TAG, "graphqlPost error", e)
            null
        }
    }

    // ------------------------------------------------------------------
    // الاستراتيجية 2: fromXdt — بيانات xdt داخل HTML صفحة المنشور
    // ------------------------------------------------------------------

    private fun fromXdt(code: String): IgResult.Video? {
        return try {
            val request = Request.Builder()
                .url("https://www.instagram.com/p/$code/")
                .header("User-Agent", UA)
                .header("Accept", "text/html,application/xhtml+xml")
                .header("X-IG-App-ID", IG_APP_ID)
                .header("Referer", "https://www.instagram.com/")
                .build()
            client.newCall(request).execute().use { resp ->
                if (!resp.isSuccessful) return null
                val html = resp.body?.string() ?: return null
                val m = Regex(""""video_url"\s*:\s*"([^"]+)"""").find(html) ?: return null
                val videoUrl = unescapeJson(m.groupValues[1])
                if (videoUrl.startsWith("http")) {
                    IgResult.Video(videoUrl, "instagram_$code")
                } else null
            }
        } catch (e: Exception) {
            Log.w(TAG, "fromXdt error", e)
            null
        }
    }

    // ------------------------------------------------------------------
    // الاستراتيجية 3: viaEmbed — صفحة الـ embed الرسمية
    // ------------------------------------------------------------------

    private fun viaEmbed(code: String): IgResult.Video? {
        return try {
            val request = Request.Builder()
                .url("https://www.instagram.com/p/$code/embed/captioned/")
                .header("User-Agent", UA)
                .header("Referer", "https://www.instagram.com/")
                .build()
            client.newCall(request).execute().use { resp ->
                if (!resp.isSuccessful) return null
                val html = resp.body?.string() ?: return null
                val m = Regex(""""video_url"\s*:\s*"([^"]+)"""").find(html) ?: return null
                val videoUrl = unescapeJson(m.groupValues[1])
                if (videoUrl.startsWith("http")) {
                    IgResult.Video(videoUrl, "instagram_$code")
                } else null
            }
        } catch (e: Exception) {
            Log.w(TAG, "viaEmbed error", e)
            null
        }
    }

    // ------------------------------------------------------------------
    // أدوات مساعدة
    // ------------------------------------------------------------------

    /** يقرأ video_url من JSON الناتج عن GraphQL (data.xdt_shortcode_media). */
    private fun parseXdtJson(json: String, code: String): IgResult.Video? {
        return try {
            val root = org.json.JSONObject(json)
            val media = root.optJSONObject("data")
                ?.optJSONObject("xdt_shortcode_media")
                ?: return null
            val videoUrl = media.optString("video_url", "")
            if (videoUrl.isNotEmpty()) {
                val title = media.optString("edge_media_to_caption", "")
                    .takeIf { it.isNotEmpty() }
                    ?.let { captionJson ->
                        runCatching {
                            org.json.JSONObject(captionJson)
                                .getJSONArray("edges").getJSONObject(0)
                                .getJSONObject("node").getString("text")
                        }.getOrNull()
                    } ?: "instagram_$code"
                return IgResult.Video(videoUrl, title.take(80))
            }
            // منشور متعدد الوسائط: خذ أول مقطع فيديو من الأطفال
            val children = media.optJSONObject("edge_sidecar_to_children")
                ?.optJSONArray("edges")
            if (children != null) {
                for (i in 0 until children.length()) {
                    val node = children.getJSONObject(i).optJSONObject("node") ?: continue
                    val childUrl = node.optString("video_url", "")
                    if (childUrl.isNotEmpty()) {
                        return IgResult.Video(childUrl, "instagram_$code")
                    }
                }
            }
            null
        } catch (e: Exception) {
            Log.w(TAG, "parseXdtJson error", e)
            null
        }
    }

    /** فك ترميز JSON (\uXXXX و \/ و \\"). */
    private fun unescapeJson(s: String): String {
        val sb = StringBuilder(s.length)
        var i = 0
        while (i < s.length) {
            val c = s[i]
            if (c == '\\' && i + 1 < s.length) {
                when (val n = s[i + 1]) {
                    'u' -> {
                        val hex = s.substring(i + 2, i + 6)
                        sb.append(hex.toInt(16).toChar())
                        i += 6
                    }
                    '/' -> { sb.append('/'); i += 2 }
                    '"' -> { sb.append('"'); i += 2 }
                    '\\' -> { sb.append('\\'); i += 2 }
                    'n' -> { sb.append('\n'); i += 2 }
                    else -> { sb.append(n); i += 2 }
                }
            } else {
                sb.append(c)
                i++
            }
        }
        return sb.toString()
    }
}
