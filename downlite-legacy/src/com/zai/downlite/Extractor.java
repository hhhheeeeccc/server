package com.zai.downlite;

import com.grack.nanojson.JsonArray;
import com.grack.nanojson.JsonObject;

import org.json.JSONArray;
import org.json.JSONObject;
import org.schabi.newpipe.extractor.NewPipe;
import org.schabi.newpipe.extractor.localization.ContentCountry;
import org.schabi.newpipe.extractor.localization.Localization;
import org.schabi.newpipe.extractor.services.youtube.YoutubeJavaScriptPlayerManager;
import org.schabi.newpipe.extractor.services.youtube.YoutubeStreamHelper;
import org.schabi.newpipe.extractor.downloader.Downloader;
import org.schabi.newpipe.extractor.downloader.Request;
import org.schabi.newpipe.extractor.downloader.Response;
import org.schabi.newpipe.extractor.exceptions.ReCaptchaException;
import org.schabi.newpipe.extractor.stream.StreamInfo;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.util.ArrayList;
import java.util.Collections;
import java.util.Comparator;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * Native extraction engine for the quick-download dialog.
 * Primary path: the visionOS player client — one innertube request that still
 * returns every video quality + audio stream without a PO token, and its
 * googlevideo URLs stream at full speed (verified 2026-11: ~11 MB/s, HTTP 206).
 * Fallback path: StreamInfo (audio-only on most videos since YouTube hardened
 * the classic clients).
 * The dialog path returns RAW googlevideo URLs (one request, ~1s) so the quality
 * sheet appears instantly even on slow networks; the download service calls
 * deThrottle() in the background to lift the n-param speed cap via the player JS.
 * Pure logic: caller supplies the background thread.
 */
public final class Extractor {

    private static volatile boolean inited = false;
    private static final Pattern CLEN = Pattern.compile("[?&]clen=(\\d+)");
    private static final Pattern VIDEO_ID = Pattern.compile(
            "(?i)(?:v=|youtu\\.be/|/shorts/|/embed/|/live/)([A-Za-z0-9_-]{11})");
    private static final Pattern INSTAGRAM = Pattern.compile(
            "(?i)instagram\\.com|instagr\\.am");

    private Extractor() {
    }

    private static synchronized void init() {
        if (!inited) {
            NewPipe.init(new HttpDownloader());
            inited = true;
        }
    }

    public static long clen(String url) {
        if (url == null) return 0;
        Matcher m = CLEN.matcher(url);
        return m.find() ? Long.parseLong(m.group(1)) : 0;
    }

    public static String sanitize(String s) {
        String r = s == null ? "video" : s.replaceAll("[\\\\/:*?\"<>|\\n\\r]", " ").trim();
        return r.isEmpty() ? "video" : r.substring(0, Math.min(60, r.length()));
    }

    /** Extract stream info for any supported URL; never throws — returns {"ok":false,"error":...} on failure. */
    public static JSONObject extract(String url) {
        if (INSTAGRAM.matcher(url).find()) {
            return InstagramExtractor.extract(url);
        }
        try {
            init();
        } catch (Throwable t) {
            return err(t);
        }
        JSONObject rejected = null;
        if (VIDEO_ID.matcher(url).find()) {
            for (int attempt = 0; attempt < 2; attempt++) {
                JSONObject v = extractVisionOs(url);
                if (v != null && v.optBoolean("ok")) return v;
                if (v != null && !v.optBoolean("ok")) {
                    rejected = v; // unavailable / live — definitive
                    break;
                }
            }
        }
        try {
            JSONObject s = extractStreamInfo(url);
            if (s.optBoolean("ok")) return s;
        } catch (Throwable ignored) {
        }
        if (rejected != null) return rejected;
        return err(new Exception("not a bot"));
    }

    private static JSONObject err(Throwable t) {
        JSONObject out = new JSONObject();
        String msg = t.getMessage() == null ? t.getClass().getSimpleName() : t.getMessage();
        if (msg.length() > 160) msg = msg.substring(0, 160);
        try {
            out.put("ok", false);
            out.put("error", msg);
        } catch (Exception ignored) {
        }
        return out;
    }

    // ------------------------------------------------------------------ primary: visionOS client

    /**
     * Rewrite the throttling parameter (n=...) of a googlevideo URL using the player JS
     * via rhino. Without this, googlevideo caps downloads at ~80 KB/s. Returns the
     * original URL if the transform cannot be computed (better a throttled download
     * than none). The player JS is fetched once and cached per process, so this is
     * cheap after the first download. Runs in the download service, never in the
     * quality-sheet path (the sheet must open in ~1 request even on slow networks).
     */
    public static String deThrottle(String vid, String url) {
        if (url == null || vid == null || vid.isEmpty()) return url;
        try {
            return YoutubeJavaScriptPlayerManager.getUrlWithThrottlingParameterDeobfuscated(vid, url);
        } catch (Throwable t) {
            return url;
        }
    }

    /**
     * One request to the visionOS player. Returns null on transport/parse failure so the
     * caller can fall back; returns ok:false with a friendly error for real rejections
     * (unavailable video, login required, live stream).
     */
    private static JSONObject extractVisionOs(String url) {
        Matcher m = VIDEO_ID.matcher(url);
        if (!m.find()) return null;
        String vid = m.group(1);
        JSONObject out = new JSONObject();
        try {
            JsonObject pr = YoutubeStreamHelper.getVisionOsPlayerResponse(
                    new ContentCountry("US"), new Localization("en", "US"), vid, null);
            JsonObject ps = pr.getObject("playabilityStatus");
            String status = ps.getString("status", "OK");
            if (!"OK".equals(status)) {
                String reason = ps.getString("reason", "");
                if ("LIVE_STREAM".equals(status) || reason.matches("(?i).*live.*")) {
                    out.put("ok", false).put("error", "البث المباشر غير مدعوم — انتظر انتهاء البث");
                    return out;
                }
                if (reason.matches("(?i).*(video|unavailable|private|removed).*")) {
                    out.put("ok", false).put("error", "الفيديو غير متاح");
                    return out;
                }
                // bot/login gates and anything else: fall through so the caller can retry via StreamInfo
                return null;
            }
            JsonObject details = pr.getObject("videoDetails");
            JsonObject sd = pr.getObject("streamingData");
            JsonArray formats = sd.getArray("adaptiveFormats");
            if (details.isEmpty() || formats == null || formats.size() == 0) return null;

            out.put("ok", true);
            out.put("vid", vid);
            out.put("title", details.getString("title", "فيديو"));
            out.put("uploader", details.getString("author", ""));
            long dur = 0;
            try {
                dur = Long.parseLong(details.getString("lengthSeconds", "0"));
            } catch (Exception ignored) {
            }
            out.put("duration", dur);

            // best AAC audio stream (m4a — mux-compatible with MediaMuxer and plays everywhere)
            String aurl = null;
            long abitrate = 0, asize = 0;
            for (int i = 0; i < formats.size(); i++) {
                JsonObject f = formats.getObject(i);
                String mime = f.getString("mimeType", "");
                if (mime.startsWith("audio/mp4")) {
                    int b = (int) f.getLong("bitrate", 0);
                if (b > abitrate) {
                    abitrate = b;
                    aurl = f.getString("url", null);
                    asize = f.getLong("contentLength", clen(aurl));
                }
                }
            }

            // video-only streams: keep H.264 in MP4 (hardware decode + MediaMuxer merge)
            List<int[]> heights = new ArrayList<int[]>(); // [height, index]
            Set<Integer> seen = new LinkedHashSet<Integer>();
            List<JsonObject> videos = new ArrayList<JsonObject>();
            for (int i = 0; i < formats.size(); i++) {
                JsonObject f = formats.getObject(i);
                String mime = f.getString("mimeType", "");
                if (mime.startsWith("video/mp4") && mime.contains("avc1")) {
                    int h = (int) f.getLong("height", 0);
                    if (h > 0 && seen.add(h)) {
                        heights.add(new int[]{h, videos.size()});
                        videos.add(f);
                    }
                }
            }
            Collections.sort(heights, new Comparator<int[]>() {
                public int compare(int[] a, int[] b) {
                    return Integer.compare(b[0], a[0]);
                }
            });

            JSONArray arr = new JSONArray();
            for (int[] e : heights) {
                JsonObject f = videos.get(e[1]);
                long vsize = f.getLong("contentLength", clen(f.getString("url", "")));
                JSONObject o = new JSONObject();
                o.put("kind", "video");
                o.put("label", "فيديو " + Ar.n(e[0]));
                o.put("mime", "video/mp4");
                o.put("url", f.getString("url", ""));
                o.put("aurl", aurl);
                o.put("size", vsize + asize);
                o.put("merge", true);
                arr.put(o);
            }
            if (aurl != null) {
                JSONObject o = new JSONObject();
                o.put("kind", "audio");
                o.put("label", "الصوت فقط " + Ar.n(Math.round(abitrate / 1000.0)));
                o.put("mime", "audio/mp4");
                o.put("url", aurl);
                o.put("size", asize);
                o.put("merge", false);
                arr.put(o);
            }
            if (arr.length() == 0) return null;
            out.put("formats", arr);
            return out;
        } catch (Throwable t) {
            return null; // transport/parse problem — let the fallback try
        }
    }

    // ------------------------------------------------------------------ fallback: StreamInfo

    private static JSONObject extractStreamInfo(String url) throws Exception {
        JSONObject out = new JSONObject();
        StreamInfo info = StreamInfo.getInfo(url);
        out.put("ok", true);
        out.put("vid", info.getId());
        out.put("title", info.getName());
        out.put("uploader", info.getUploaderName() == null ? "" : info.getUploaderName());
        out.put("duration", info.getDuration());

        List<org.schabi.newpipe.extractor.stream.VideoStream> vs =
                new ArrayList<org.schabi.newpipe.extractor.stream.VideoStream>(info.getVideoStreams());
        Collections.sort(vs, new Comparator<org.schabi.newpipe.extractor.stream.VideoStream>() {
            public int compare(org.schabi.newpipe.extractor.stream.VideoStream a,
                               org.schabi.newpipe.extractor.stream.VideoStream b) {
                return Integer.compare(b.getHeight(), a.getHeight());
            }
        });

        org.schabi.newpipe.extractor.MediaFormat want = org.schabi.newpipe.extractor.MediaFormat.M4A;
        org.schabi.newpipe.extractor.stream.AudioStream bestA = null;
        for (org.schabi.newpipe.extractor.stream.AudioStream a : info.getAudioStreams()) {
            if (a.getFormat() == want && (bestA == null || a.getAverageBitrate() > bestA.getAverageBitrate())) {
                bestA = a;
            }
        }

        JSONArray formats = new JSONArray();
        Set<String> labels = new LinkedHashSet<String>();
        for (org.schabi.newpipe.extractor.stream.VideoStream v : vs) {
            String label = v.getResolution();
            if (label == null || label.isEmpty()) label = String.valueOf(v.getHeight());
            if (label.endsWith("p")) label = label.substring(0, label.length() - 1);
            label = "فيديو " + Ar.n(label);
            if (!labels.add(label)) continue;
            JSONObject f = new JSONObject();
            f.put("kind", "video");
            f.put("label", label);
            f.put("mime", "video/mp4");
            f.put("url", v.getUrl());
            f.put("size", clen(v.getUrl()));
            f.put("merge", bestA != null && v.isVideoOnly());
            if (bestA != null && v.isVideoOnly()) f.put("aurl", bestA.getUrl());
            formats.put(f);
        }
        if (bestA != null) {
            JSONObject f = new JSONObject();
            f.put("kind", "audio");
            f.put("label", "الصوت فقط " + Ar.n(bestA.getAverageBitrate()));
            f.put("mime", "audio/mp4");
            f.put("url", bestA.getUrl());
            f.put("size", clen(bestA.getUrl()));
            f.put("merge", false);
            formats.put(f);
        }
        out.put("formats", formats);
        return out;
    }

    // ------------------------------------------------------------------ HTTP layer

    static class HttpDownloader extends Downloader {
        @Override
        public Response execute(Request req) throws IOException, ReCaptchaException {
            HttpURLConnection c = (HttpURLConnection) new URL(req.url()).openConnection();
            c.setConnectTimeout(10000);
            c.setReadTimeout(25000);
            c.setRequestMethod(req.httpMethod());
            Map<String, List<String>> hs = req.headers();
            if (hs != null) {
                for (Map.Entry<String, List<String>> e : hs.entrySet()) {
                    if (e.getValue() != null && !e.getValue().isEmpty()) {
                        c.setRequestProperty(e.getKey(), e.getValue().get(0));
                    }
                }
            }
            byte[] data = req.dataToSend();
            if (data != null) {
                c.setDoOutput(true);
                c.setFixedLengthStreamingMode(data.length);
                OutputStream os = c.getOutputStream();
                os.write(data);
                os.close();
            }
            int code = c.getResponseCode();
            InputStream is = code < 400 ? c.getInputStream() : c.getErrorStream();
            ByteArrayOutputStream ba = new ByteArrayOutputStream();
            byte[] buf = new byte[8192];
            int n;
            if (is != null) {
                while ((n = is.read(buf)) > 0) {
                    ba.write(buf, 0, n);
                }
                is.close();
            }
            String body = ba.toString("UTF-8");
            if (code == 429) {
                throw new ReCaptchaException("reCaptcha Challenge requested", req.url());
            }
            return new Response(code, c.getResponseMessage(), c.getHeaderFields(), body, c.getURL().toString());
        }
    }
}
