package com.zai.downlite;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.net.URLDecoder;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * Native Instagram extractor (reels / posts / IGTV, video or image), layered attempts.
 * v1.9: an optional in-app session (IgSession, captured via IgLoginActivity) is sent
 * with every attempt — Instagram now demands login for many public posts, which is
 * exactly the «أنا مسجّل دخولي» complaint: the Instagram app's session is sandboxed
 * and never shared, so حمّل carries its own.
 *
 *  1. The official Android app API  i.instagram.com/api/v1/media/{pk}/info/ with real
 *     Instagram app headers (best when a session exists; anonymous it usually fails fast).
 *  2. The web GraphQL endpoint www.instagram.com/api/graphql with LSD + CSRF tokens from
 *     the homepage — TWO queries: first the mobile-web post loader (doc_id used by
 *     instagram.com itself; public posts return xdt_shortcode_media WITHOUT any login,
 *     verified 2026-11), then the logged-out desktop query as a fallback.
 *  3. The /embed/captioned/ page — anonymous static HTML that often carries the URL.
 *  4. Legacy HTML scrape of the post page (escaped "video_url" / "display_url").
 *
 * Output contract matches Extractor: {"ok":true,"title":..,"vid":"",
 * "formats":[{"kind":"video"|"image","label":..,"size":0,"url":..,"mime":..}]}
 * or {"ok":false,"error":<arabic>}.
 *
 * Known limits (honest errors, never silent): private accounts and stories need a
 * login; datacenter-IP TLS gating can refuse every attempt with a clear message.
 * Pure logic: caller supplies the background thread. Never throws.
 */
public final class InstagramExtractor {

    private static final String UA_APP = "Instagram 275.0.0.27.98 Android (33/13; 420dpi; "
            + "1080x2400; samsung; SM-G991B; o1s; exynos2100; en_US; 458229237)";
    private static final String UA_WEB = "Mozilla/5.0 (Linux; Android 13; Pixel 7) "
            + "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Mobile Safari/537.36";
    private static final String WEB_APP_ID = "936619743392459";
    private static final String ANDROID_APP_ID = "567067343352989";
    private static final String GRAPHQL_DOC_ID = "27130156389949648";
    private static final String FRIENDLY_NAME = "PolarisLoggedOutDesktopWWWPostRootContentQuery";
    // mobile-web post loader — the query instagram.com itself fires for visitors:
    // public posts answer anonymously (rate-limit keyed per IP, not per account)
    private static final String GRAPHQL_DOC_ID_MW = "8845758582119845";
    private static final String FRIENDLY_NAME_MW = "PolarisPostActionLoadPostQueryQuery";

    private static final Pattern SHORTCODE = Pattern.compile(
            "(?i)instagram\\.com/(?:reel|p|tv)/([A-Za-z0-9_-]{5,})");
    private static final Pattern SHARE_LINK = Pattern.compile(
            "(?i)instagram\\.com/share/(?:reel|p)?/?([A-Za-z0-9_-]{3,})");

    private InstagramExtractor() {
    }

    /** shortcode → numeric media pk (yt-dlp _id_to_pk: base64-N decode, alphabet A-Za-z0-9-_). */
    public static String pkOf(String code) {
        if (code == null) return null;
        if (code.length() > 28) code = code.substring(0, code.length() - 28);
        final String A = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
        long pk = 0;
        for (int i = 0; i < code.length(); i++) {
            int idx = A.indexOf(code.charAt(i));
            if (idx < 0) return null;
            pk = pk * 64 + idx;
        }
        return pk > 0 ? Long.toString(pk) : null;
    }

    /** Instagram shortcode from any share text; /share/ links are resolved via redirect. */
    public static String shortcode(String url) {
        if (url == null) return null;
        Matcher m = SHORTCODE.matcher(url);
        if (m.find()) return m.group(1);
        m = SHARE_LINK.matcher(url);
        if (m.find()) {
            String canon = followRedirect(url);
            if (canon != null) {
                m = SHORTCODE.matcher(canon);
                if (m.find()) return m.group(1);
            }
        }
        return null;
    }

    public static JSONObject extract(String url) {
        try {
            String code = shortcode(url);
            if (code == null) {
                return err("رابط إنستغرام غير مفهوم — شارك المنشور مرة أخرى من تطبيق إنستغرام");
            }
            String pk = pkOf(code);
            JSONObject item = null;
            String[] apiErr = new String[1]; // api error body (login_required, …)
            if (pk != null) {
                item = viaAppApi(pk, apiErr);
            }
            if (item == null) {
                item = viaGraphQl(pk, code);
            }
            if (item == null) {
                item = viaEmbed(code);
            }
            if (item == null) {
                item = viaLegacyHtml(code);
            }
            if (item == null) {
                if (apiErr[0] != null && apiErr[0].toLowerCase().contains("login_required")) {
                    return needsLogin(IgSession.has()
                            ? "انتهت جلسة إنستغرام المحفوظة داخل نزّل الفيديو — حدّث الجلسة ثم أعد المحاولة"
                            : "هذا المنشور يحتاج جلسة إنستغرام (المنشورات العامة تُحمَّل بدونها) — إن أردت، سجّل الدخول داخل «نزّل الفيديو» مرة واحدة فقط");
                }
                return err("إنستغرام رفض إعطاء بيانات الفيديو (حماية ضد التحميل) — حاول بعد قليل أو استخدم منشورًا عامًا");
            }
            return build(item, code);
        } catch (Throwable t) {
            return err("فشل التحليل: " + (t.getMessage() == null ? t.getClass().getSimpleName() : t.getMessage()));
        }
    }

    // ------------------------------------------------------------------ attempts

    /** Attempt 1 — official Android app API with real app headers. apiErr[0] gets the API error body. */
    private static JSONObject viaAppApi(String pk, String[] apiErr) {
        try {
            HttpURLConnection c = open("https://i.instagram.com/api/v1/media/" + pk + "/info/", "GET");
            c.setRequestProperty("User-Agent", UA_APP);
            c.setRequestProperty("X-IG-App-ID", ANDROID_APP_ID);
            c.setRequestProperty("X-IG-Capabilities", "3brTvw8=");
            c.setRequestProperty("X-IG-Connection-Type", "WIFI");
            c.setRequestProperty("X-FB-HTTP-Engine", "Liger");
            c.setRequestProperty("Accept", "*/*");
            c.setRequestProperty("Accept-Language", "en-US");
            if (IgSession.has()) c.setRequestProperty("Cookie", IgSession.cookie()); // v1.9: logged-in session
            String body = read(c);
            if (body == null) return null;
            JSONObject j = new JSONObject(body);
            if ("fail".equalsIgnoreCase(j.optString("status"))) {
                apiErr[0] = j.optString("message", "") + " " + j.optString("error_title", "");
                return null;
            }
            JSONArray items = j.optJSONArray("items");
            return items != null && items.length() > 0 ? items.getJSONObject(0) : null;
        } catch (Throwable t) {
            return null;
        }
    }

    /** Attempt 2 — web GraphQL with tokens scraped from the homepage. */
    private static JSONObject viaGraphQl(String pk, String code) {
        try {
            // homepage: grab LSD token + csrftoken cookie
            HttpURLConnection h = open("https://www.instagram.com/", "GET");
            h.setRequestProperty("User-Agent", UA_WEB);
            h.setRequestProperty("Accept-Language", "en-US,en;q=0.9");
            if (IgSession.has()) h.setRequestProperty("Cookie", IgSession.cookie()); // v1.9: logged-in session
            String home = read(h);
            if (home == null) return null;
            String lsd = null;
            Matcher m = Pattern.compile("\\[\"LSD\",\\[\\],\\{\"token\":\"([^\"]+)\"").matcher(home);
            if (m.find()) lsd = m.group(1);
            String csrf = cookie(h.getHeaderFields().get("Set-Cookie"), "csrftoken");
            if (csrf == null && IgSession.has()) {
                csrf = cookieVal(IgSession.cookie(), "csrftoken"); // fall back to the stored session
            }
            if (lsd == null || csrf == null) return null;

            String postUrl = "https://www.instagram.com/p/" + code + "/";
            // cookie: the stored session when present, otherwise just the homepage csrftoken
            String ckh = IgSession.has() ? IgSession.cookie() : "";
            if (!ckh.contains("csrftoken=")) {
                ckh = (ckh.isEmpty() ? "" : ckh + "; ") + "csrftoken=" + csrf;
            }

            // Attempt A — the mobile-web post loader (the query instagram.com itself
            // fires for anonymous visitors): public posts return data without any login
            JSONObject anon = graphqlPost(postUrl, ckh, csrf, lsd,
                    GRAPHQL_DOC_ID_MW, FRIENDLY_NAME_MW,
                    "{\"shortcode\":\"" + code + "\",\"fetch_tagged_user_count\":null,"
                            + "\"hoisted_comment_id\":null,\"hoisted_reply_id\":null}",
                    "129477", true);
            if (anon != null) return anon;

            // Attempt B — the logged-out desktop query (app-API shaped response)
            return graphqlPost(postUrl, ckh, csrf, lsd,
                    GRAPHQL_DOC_ID, FRIENDLY_NAME,
                    "{\"media_id\":\"" + pk + "\"}", "359341", false);
        } catch (Throwable t) {
            return null;
        }
    }

    /** One POST to instagram.com/api/graphql with the homepage tokens; never throws. */
    private static JSONObject graphqlPost(String postUrl, String ckh, String csrf, String lsd,
            String docId, String friendly, String variables, String asbd, boolean xdtShape) {
        try {
            HttpURLConnection c = open("https://www.instagram.com/api/graphql", "POST");
            c.setRequestProperty("User-Agent", UA_WEB);
            c.setRequestProperty("Cookie", ckh);
            c.setRequestProperty("X-IG-App-ID", WEB_APP_ID);
            c.setRequestProperty("X-ASBD-ID", asbd);
            c.setRequestProperty("X-IG-WWW-Claim", "0");
            c.setRequestProperty("Origin", "https://www.instagram.com");
            c.setRequestProperty("Accept", "*/*");
            c.setRequestProperty("Sec-Fetch-Site", "same-origin");
            c.setRequestProperty("Sec-Fetch-Mode", "cors");
            c.setRequestProperty("Sec-Fetch-Dest", "empty");
            c.setRequestProperty("X-FB-Friendly-Name", friendly);
            c.setRequestProperty("X-CSRFToken", csrf);
            c.setRequestProperty("X-FB-LSD", lsd);
            c.setRequestProperty("Referer", postUrl);
            c.setRequestProperty("Content-Type", "application/x-www-form-urlencoded");
            StringBuilder b = new StringBuilder();
            b.append("av=0&__d=www&__user=0&__a=1&__req=k&dpr=1&__ccg=UNKNOWN&__comet_req=7")
             .append("&lsd=").append(enc(lsd))
             .append("&jazoest=2957&__spin_r=1008408688&__spin_t=1739300000&__spin_b=trunk&__origin=www")
             .append("&fb_api_caller_class=RelayModern")
             .append("&fb_api_req_friendly_name=").append(friendly)
             .append("&server_timestamps=true")
             .append("&variables=").append(enc(variables))
             .append("&doc_id=").append(docId);
            OutputStream os = c.getOutputStream();
            os.write(b.toString().getBytes("UTF-8"));
            os.close();
            String body = read(c);
            if (body == null) return null;
            JSONObject j = new JSONObject(body);
            JSONObject data = j.optJSONObject("data");
            if (data == null) return null; // {"errors":[…]} → let the next attempt try
            if (xdtShape) {
                return fromXdt(data.optJSONObject("xdt_shortcode_media"));
            }
            JSONObject media = data.optJSONObject("xig_polaris_media");
            return media == null ? null : media.optJSONObject("if_not_gated_logged_out");
        } catch (Throwable t) {
            return null;
        }
    }

    /** Normalize the mobile-web GraphQL shape (xdt_shortcode_media) into the app-API
     *  shape build() expects (video_versions / image_versions2 / carousel_media). */
    private static JSONObject fromXdt(JSONObject sm) {
        if (sm == null) return null;
        try {
            JSONObject o = new JSONObject();
            String vu = sm.optString("video_url", "");
            if (vu.startsWith("http")) {
                o.put("video_versions", new JSONArray().put(
                        new JSONObject().put("url", vu).put("type", "video/mp4")));
            }
            String du = sm.optString("display_url", "");
            if (du.startsWith("http")) {
                o.put("image_versions2", new JSONObject().put("candidates",
                        new JSONArray().put(new JSONObject().put("url", du))));
            }
            JSONObject sc = sm.optJSONObject("edge_sidecar_to_children");
            JSONArray edges = sc == null ? null : sc.optJSONArray("edges");
            if (edges != null && edges.length() > 0) {
                JSONArray kids = new JSONArray();
                for (int i = 0; i < edges.length(); i++) {
                    JSONObject node = edges.getJSONObject(i).optJSONObject("node");
                    if (node == null) continue;
                    JSONObject k = new JSONObject();
                    String nv = node.optString("video_url", "");
                    if (nv.startsWith("http")) {
                        k.put("video_versions", new JSONArray().put(
                                new JSONObject().put("url", nv).put("type", "video/mp4")));
                    }
                    String nd = node.optString("display_url", "");
                    if (nd.startsWith("http")) {
                        k.put("image_versions2", new JSONObject().put("candidates",
                                new JSONArray().put(new JSONObject().put("url", nd))));
                    }
                    if (k.length() > 0) kids.put(k);
                }
                if (kids.length() > 0) o.put("carousel_media", kids);
            }
            if (o.length() == 0) return null;
            JSONObject capBox = sm.optJSONObject("edge_media_to_caption");
            JSONArray capEdges = capBox == null ? null : capBox.optJSONArray("edges");
            if (capEdges != null && capEdges.length() > 0) {
                String text = capEdges.getJSONObject(0).optJSONObject("node").optString("text", "");
                if (!text.isEmpty()) o.put("caption", new JSONObject().put("text", text));
            }
            JSONObject owner = sm.optJSONObject("owner");
            if (owner != null && !owner.optString("username", "").isEmpty()) {
                o.put("user", new JSONObject().put("username", owner.optString("username")));
            }
            return o;
        } catch (Throwable t) {
            return null;
        }
    }

    /** Attempt 3 — the /embed/captioned/ page: anonymous static HTML that often carries
     *  the media URL even when the post page itself is login-walled. */
    private static JSONObject viaEmbed(String code) {
        return scrape("https://www.instagram.com/p/" + code + "/embed/captioned/");
    }

    /** Attempt 4 — legacy escaped scrape of the post page. */
    private static JSONObject viaLegacyHtml(String code) {
        return scrape("https://www.instagram.com/p/" + code + "/");
    }

    private static JSONObject scrape(String url) {
        try {
            HttpURLConnection c = open(url, "GET");
            c.setRequestProperty("User-Agent", UA_WEB);
            c.setRequestProperty("Accept-Language", "en-US,en;q=0.9");
            if (IgSession.has()) c.setRequestProperty("Cookie", IgSession.cookie()); // v1.9
            String html = read(c);
            if (html == null) return null;
            String video = unescapeJson(html, "video_url");
            if (video != null) {
                JSONObject o = new JSONObject();
                o.put("video_versions", new JSONArray().put(
                        new JSONObject().put("url", video).put("type", "video/mp4")));
                return o;
            }
            String image = unescapeJson(html, "display_url");
            if (image != null) {
                JSONObject o = new JSONObject();
                o.put("image_versions2", new JSONObject().put("candidates", new JSONArray().put(
                        new JSONObject().put("url", image))));
                return o;
            }
            return null;
        } catch (Throwable t) {
            return null;
        }
    }

    // ------------------------------------------------------------------ result building

    /** Flatten one media item (app API shape) into the Extractor contract. */
    private static JSONObject build(JSONObject item, String code) {
        try {
            // carousel: take the first child that carries media
            JSONArray carousel = item.optJSONArray("carousel_media");
            if (carousel != null && carousel.length() > 0) {
                item = carousel.getJSONObject(0);
            }
            JSONObject out = new JSONObject();
            out.put("ok", true);
            out.put("vid", "");
            JSONArray formats = new JSONArray();

            JSONArray vv = item.optJSONArray("video_versions");
            if (vv != null && vv.length() > 0) {
                // dedupe by height, best first (the list is ordered best→worst)
                Map<Integer, JSONObject> best = new LinkedHashMap<>();
                for (int i = 0; i < vv.length(); i++) {
                    JSONObject v = vv.optJSONObject(i);
                    if (v == null) continue;
                    int h = v.optInt("height", 0);
                    int key = h == 0 ? 100000 + i : h;
                    if (!best.containsKey(key)) best.put(key, v);
                }
                ArrayList<Integer> keys = new ArrayList<>(best.keySet());
                java.util.Collections.sort(keys, new java.util.Comparator<Integer>() {
                    public int compare(Integer a, Integer b) {
                        return b - a; // tallest first
                    }
                });
                for (int h : keys) {
                    JSONObject v = best.get(h);
                    JSONObject f = new JSONObject();
                    f.put("kind", "video");
                    f.put("label", h > 0 ? "فيديو " + Ar.n(h) : "فيديو");
                    f.put("size", 0);
                    f.put("url", v.optString("url", ""));
                    f.put("mime", v.optString("type", "video/mp4"));
                    formats.put(f);
                }
            } else {
                JSONObject im = item.optJSONObject("image_versions2");
                JSONArray cand = im == null ? null : im.optJSONArray("candidates");
                if (cand != null && cand.length() > 0) {
                    JSONObject f = new JSONObject();
                    f.put("kind", "image");
                    f.put("label", "صورة");
                    f.put("size", 0);
                    f.put("url", cand.getJSONObject(0).optString("url", ""));
                    f.put("mime", "image/jpeg");
                    formats.put(f);
                }
            }
            if (formats.length() == 0) {
                return err("هذا المنشور لا يحتوي فيديو أو صورة قابلة للتحميل");
            }
            String title = null;
            JSONObject cap = item.optJSONObject("caption");
            if (cap != null) title = cap.optString("text", null);
            if (title == null || title.trim().isEmpty()) {
                JSONObject user = item.optJSONObject("user");
                title = (user == null ? "" : user.optString("username", "")) + " " + code;
            }
            out.put("title", Extractor.sanitize(title));
            out.put("formats", formats);
            return out;
        } catch (Throwable t) {
            return err("تعذر قراءة بيانات المنشور");
        }
    }

    private static JSONObject err(String msg) {
        JSONObject o = new JSONObject();
        try {
            o.put("ok", false);
            o.put("error", msg);
        } catch (Throwable ignored) {
        }
        return o;
    }

    /** Failure that one in-app Instagram login (IgLoginActivity) fixes — the UI shows a login button. */
    private static JSONObject needsLogin(String msg) {
        JSONObject o = err(msg);
        try {
            o.put("needsLogin", true);
        } catch (Throwable ignored) {
        }
        return o;
    }

    // ------------------------------------------------------------------ http helpers

    private static HttpURLConnection open(String url, String method) throws IOException {
        HttpURLConnection c = (HttpURLConnection) new URL(url).openConnection();
        c.setConnectTimeout(8000);
        c.setReadTimeout(10000);
        c.setRequestMethod(method);
        c.setInstanceFollowRedirects(true);
        if ("POST".equals(method)) c.setDoOutput(true);
        return c;
    }

    private static String read(HttpURLConnection c) throws IOException {
        int code = c.getResponseCode();
        InputStream is = code < 400 ? c.getInputStream() : c.getErrorStream();
        if (is == null) return null;
        ByteArrayOutputStream ba = new ByteArrayOutputStream();
        byte[] buf = new byte[8192];
        int n;
        while ((n = is.read(buf)) > 0) ba.write(buf, 0, n);
        is.close();
        if (code == 200 || code == 201) return ba.toString("UTF-8");
        // keep API error bodies (they carry login_required etc.)
        String s = ba.toString("UTF-8");
        return s.trim().startsWith("{") ? s : null;
    }

    private static String followRedirect(String url) {
        try {
            HttpURLConnection c = open(url, "GET");
            c.setRequestProperty("User-Agent", UA_WEB);
            read(c);
            String finalUrl = c.getURL().toString();
            c.disconnect();
            return finalUrl;
        } catch (Throwable t) {
            return null;
        }
    }

    private static String cookie(java.util.List<String> setCookies, String name) {
        if (setCookies == null) return null;
        for (String h : setCookies) {
            for (String part : h.split(";")) {
                String p = part.trim();
                if (p.startsWith(name + "=")) return p.substring(name.length() + 1);
            }
        }
        return null;
    }

    /** Named cookie value out of a stored "a=1; b=2" Cookie header. */
    private static String cookieVal(String header, String name) {
        if (header == null) return null;
        for (String part : header.split(";")) {
            String p = part.trim();
            if (p.startsWith(name + "=")) return p.substring(name.length() + 1);
        }
        return null;
    }

    private static String enc(String s) throws IOException {
        return java.net.URLEncoder.encode(s, "UTF-8");
    }

    /** Pull an escaped string field (\"video_url\":\"…\") out of a JSON-in-HTML blob. */
    private static String unescapeJson(String html, String field) {
        Matcher m = Pattern.compile("\\\\\"" + field + "\\\\\":\\\\\"(https:[^\\\\\"]+)")
                .matcher(html);
        if (m.find()) {
            return m.group(1)
                    .replace("\\u0026", "&")
                    .replace("\\/", "/");
        }
        m = Pattern.compile("\"" + field + "\":\"(https:[^\"]+)").matcher(html);
        if (m.find()) {
            return m.group(1).replace("\\u0026", "&").replace("\\/", "/");
        }
        return null;
    }
}
