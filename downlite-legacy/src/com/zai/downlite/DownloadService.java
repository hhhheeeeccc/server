package com.zai.downlite;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.Service;
import android.content.ContentValues;
import android.content.Context;
import android.content.Intent;
import android.media.MediaCodec;
import android.media.MediaExtractor;
import android.media.MediaFormat;
import android.media.MediaMuxer;
import android.net.Uri;
import android.os.Build;
import android.os.Environment;
import android.os.IBinder;
import android.provider.MediaStore;

import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.ByteBuffer;
import java.util.concurrent.atomic.AtomicInteger;

/**
 * Foreground download service: fetches parts (with progress notification),
 * merges video+audio via MediaMuxer, saves to MediaStore Downloads.
 * Survives the quick dialog closing; shows final result as a notification.
 */
public class DownloadService extends Service {

    public static final String EXTRA_VID = "id";
    public static final String EXTRA_V = "v";
    public static final String EXTRA_A = "a";
    public static final String EXTRA_NAME = "n";
    public static final String EXTRA_MIME = "m";
    private static final String CH = "downlite_dl";
    private static final AtomicInteger JOBS = new AtomicInteger(0);

    private NotificationManager nm;

    public static void start(Context ctx, String vid, String vUrl, String aUrl, String name, String mime) {
        Intent i = new Intent(ctx, DownloadService.class);
        if (vid != null && !vid.isEmpty()) i.putExtra(EXTRA_VID, vid);
        i.putExtra(EXTRA_V, vUrl);
        if (aUrl != null) i.putExtra(EXTRA_A, aUrl);
        i.putExtra(EXTRA_NAME, name);
        i.putExtra(EXTRA_MIME, mime == null ? "video/mp4" : mime);
        if (Build.VERSION.SDK_INT >= 26) {
            ctx.startForegroundService(i);
        } else {
            ctx.startService(i);
        }
    }

    @Override
    public void onCreate() {
        super.onCreate();
        IgSession.init(this);
        nm = (NotificationManager) getSystemService(NOTIFICATION_SERVICE);
        NotificationChannel ch = new NotificationChannel(CH, "تنزيلات نزّل الفيديو", NotificationManager.IMPORTANCE_DEFAULT);
        ch.setShowBadge(true);
        nm.createNotificationChannel(ch);
    }

    @Override
    public int onStartCommand(final Intent intent, int flags, final int startId) {
        if (intent == null || intent.getStringExtra(EXTRA_V) == null) {
            stopSelf();
            return START_NOT_STICKY;
        }
        final int notifId = (int) (System.currentTimeMillis() & 0x7fffffff);
        startForeground(notifId, build("جارٍ التحضير…", 0, true, false));
        JOBS.incrementAndGet();
        new Thread(new Runnable() {
            public void run() {
                try {
                    job(intent, notifId);
                } finally {
                    if (JOBS.decrementAndGet() == 0) {
                        stopSelf();
                    }
                }
            }
        }).start();
        return START_NOT_STICKY;
    }

    private void job(Intent intent, int notifId) {
        String vid = intent.getStringExtra(EXTRA_VID);
        String vUrl = intent.getStringExtra(EXTRA_V);
        String aUrl = intent.getStringExtra(EXTRA_A);
        String name = intent.getStringExtra(EXTRA_NAME);
        String mime = intent.getStringExtra(EXTRA_MIME);
        File vf = null, af = null, out = null;
        try {
            // lift the n-param speed cap via the player JS (cached per process) —
            // raw URLs still work, only slower, so failure here is not fatal
            vUrl = Extractor.deThrottle(vid, vUrl);
            if (aUrl != null) aUrl = Extractor.deThrottle(vid, aUrl);
            vf = httpDownload(vUrl, "dl_v_" + System.currentTimeMillis(), "الفيديو", notifId);
            if (aUrl != null) {
                af = httpDownload(aUrl, "dl_a_" + System.currentTimeMillis(), "الصوت", notifId);
                notify(nm, notifId, build("جارٍ دمج الفيديو مع الصوت…", 0, true, false));
                out = new File(getCacheDir(), "merge_" + System.currentTimeMillis() + ".mp4");
                if (!mergeStreams(vf.getAbsolutePath(), af.getAbsolutePath(), out)) {
                    throw new IOException("تعذر الدمج");
                }
                save(out, name, "video/mp4");
            } else {
                save(vf, name, mime);
            }
            notify(nm, notifId, buildDone("اكتمل الحفظ في مجلد «نزّل الفيديو»: " + name));
        } catch (final Throwable t) {
            String msg = t.getMessage() == null ? "خطأ غير معروف" : t.getMessage();
            if (msg.length() > 120) msg = msg.substring(0, 120);
            String m = "فشل التنزيل — " + msg;
            notify(nm, notifId, buildDone(m));
        } finally {
            cleanup(vf, af, out);
        }
    }

    // ------------------------------------------------------------------ download / merge / save

    private static final int CHUNK = 4 << 20; // 4 MB per ranged request — fresh connections stay fast
    private static final String UA_BROWSER = "Mozilla/5.0 (Linux; Android 13; Pixel 7) "
            + "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Mobile Safari/537.36";

    /** Instagram/Facebook CDNs reject unknown agents and hot-linking — send browser headers there.
     *  v1.9: the in-app Instagram session rides along (some IG media needs the cookies). */
    private static void headersFor(HttpURLConnection c, String url) {
        String u = url == null ? "" : url.toLowerCase();
        if (u.contains("cdninstagram") || u.contains("fbcdn") || u.contains("instagram.com")) {
            c.setRequestProperty("User-Agent", UA_BROWSER);
            c.setRequestProperty("Referer", "https://www.instagram.com/");
            if (IgSession.has()) c.setRequestProperty("Cookie", IgSession.cookie());
        } else {
            c.setRequestProperty("User-Agent", "NazzilVideo/2.0");
        }
    }

    private File httpDownload(String url, String fname, String phase, int notifId) throws IOException {
        File out = new File(getCacheDir(), fname);
        FileOutputStream fo = new FileOutputStream(out);
        long total = Extractor.clen(url);
        long got = 0;
        int lastPct = -1, lastMb = 0;
        if (total > 0) {
            // chunked ranged download: one fresh connection per 4 MB
            while (got < total) {
                long end = Math.min(got + CHUNK, total) - 1;
                HttpURLConnection c = (HttpURLConnection) new URL(url).openConnection();
                c.setConnectTimeout(15000);
                c.setReadTimeout(60000);
                headersFor(c, url);
                c.setRequestProperty("Range", "bytes=" + got + "-" + end);
                int code = c.getResponseCode();
                if (code != 206 && !(code == 200 && got == 0)) {
                    c.disconnect();
                    if (code == 416 && got > 0) break; // reached the end
                    throw new IOException("HTTP " + code);
                }
                String ctype = c.getContentType();
                if (got == 0 && ctype != null && ctype.toLowerCase().contains("text/html")) {
                    c.disconnect();
                    fo.close();
                    out.delete();
                    throw new IOException("هذا الرابط صفحة ويب وليس ملفًا — لا يمكن تنزيله مباشرة");
                }
                InputStream in = c.getInputStream();
                byte[] buf = new byte[1 << 16];
                int n;
                while ((n = in.read(buf)) > 0) {
                    fo.write(buf, 0, n);
                    got += n;
                    int pct = (int) (got * 100 / total);
                    if (pct >= lastPct + 2) {
                        lastPct = pct;
                        notify(nm, notifId, build("جارٍ تنزيل " + phase + "… " + Ar.n(pct) + "٪", pct, false, false));
                    }
                }
                in.close();
                c.disconnect();
                if (code == 200) break; // server ignored Range and sent everything
            }
        } else {
            // unknown size: single plain GET, progress by MB
            HttpURLConnection c = (HttpURLConnection) new URL(url).openConnection();
            c.setConnectTimeout(15000);
            c.setReadTimeout(60000);
            headersFor(c, url);
            String ctype = c.getContentType();
            if (ctype != null && ctype.toLowerCase().contains("text/html")) {
                c.disconnect();
                fo.close();
                out.delete();
                throw new IOException("هذا الرابط صفحة ويب وليس ملفًا — لا يمكن تنزيله مباشرة");
            }
            InputStream in = c.getInputStream();
            byte[] buf = new byte[1 << 16];
            int n;
            while ((n = in.read(buf)) > 0) {
                fo.write(buf, 0, n);
                got += n;
                if (got >= (lastMb + 4) << 20) {
                    lastMb = (int) (got >> 20);
                    notify(nm, notifId, build("جارٍ تنزيل " + phase + "… " + Ar.n(lastMb) + " م.ب", 0, true, false));
                }
            }
            in.close();
            c.disconnect();
        }
        fo.close();
        // safety net: some servers lie about the type — never save an HTML page as media
        if (isHtml(out)) {
            out.delete();
            throw new IOException("هذا الرابط صفحة ويب وليس ملفًا — لا يمكن تنزيله مباشرة");
        }
        if (out.length() == 0) throw new IOException("ملف فارغ");
        return out;
    }

    private static boolean isHtml(File f) throws IOException {
        long len = f.length();
        if (len < 16) return false;
        FileInputStream fi = new FileInputStream(f);
        byte[] b = new byte[(int) Math.min(512, len)];
        int n = fi.read(b);
        fi.close();
        String s = new String(b, 0, Math.max(0, n), "UTF-8").toLowerCase();
        return s.contains("<!doctype") || s.contains("<html");
    }

    private static int pickTrack(MediaExtractor ex, String mimePrefix) {
        for (int i = 0; i < ex.getTrackCount(); i++) {
            MediaFormat f = ex.getTrackFormat(i);
            String mime = f.getString(MediaFormat.KEY_MIME);
            if (mime != null && mime.startsWith(mimePrefix)) return i;
        }
        return -1;
    }

    private boolean mergeStreams(String videoPath, String audioPath, File outFile) {
        MediaExtractor ve = null, ae = null;
        MediaMuxer muxer = null;
        try {
            ve = new MediaExtractor();
            ve.setDataSource(videoPath);
            int vTrack = pickTrack(ve, "video/");
            if (vTrack < 0) return false;
            MediaFormat vf = ve.getTrackFormat(vTrack);

            ae = new MediaExtractor();
            ae.setDataSource(audioPath);
            int aTrack = pickTrack(ae, "audio/");
            if (aTrack < 0) return false;
            MediaFormat af = ae.getTrackFormat(aTrack);

            muxer = new MediaMuxer(outFile.getAbsolutePath(), MediaMuxer.OutputFormat.MUXER_OUTPUT_MPEG_4);
            int mv = muxer.addTrack(vf);
            int ma = muxer.addTrack(af);
            muxer.start();

            int vMax = vf.containsKey(MediaFormat.KEY_MAX_INPUT_SIZE) ? vf.getInteger(MediaFormat.KEY_MAX_INPUT_SIZE) : 1 << 20;
            int aMax = af.containsKey(MediaFormat.KEY_MAX_INPUT_SIZE) ? af.getInteger(MediaFormat.KEY_MAX_INPUT_SIZE) : 1 << 20;
            ByteBuffer bb = ByteBuffer.allocate(Math.max(Math.max(vMax, aMax), 1 << 20));
            MediaCodec.BufferInfo bi = new MediaCodec.BufferInfo();

            ve.selectTrack(vTrack);
            while (true) {
                int sz = ve.readSampleData(bb, 0);
                if (sz < 0) break;
                bi.offset = 0;
                bi.size = sz;
                bi.presentationTimeUs = ve.getSampleTime();
                bi.flags = ve.getSampleFlags();
                muxer.writeSampleData(mv, bb, bi);
                ve.advance();
            }
            ae.selectTrack(aTrack);
            while (true) {
                int sz = ae.readSampleData(bb, 0);
                if (sz < 0) break;
                bi.offset = 0;
                bi.size = sz;
                bi.presentationTimeUs = ae.getSampleTime();
                bi.flags = ae.getSampleFlags();
                muxer.writeSampleData(ma, bb, bi);
                ae.advance();
            }
            return outFile.length() > 0;
        } catch (Exception e) {
            return false;
        } finally {
            try {
                if (ve != null) ve.release();
                if (ae != null) ae.release();
                if (muxer != null) muxer.release();
            } catch (Exception ignored) {
            }
        }
    }

    private void save(File src, String name, String mime) throws IOException {
        ContentValues cv = new ContentValues();
        cv.put(MediaStore.MediaColumns.DISPLAY_NAME, name);
        cv.put(MediaStore.MediaColumns.MIME_TYPE, mime);
        // folder named after the app: Download/نزّل الفيديو
        cv.put(MediaStore.MediaColumns.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS + "/نزّل الفيديو");
        Uri uri = getContentResolver().insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, cv);
        if (uri == null) throw new IOException("تعذر إنشاء الملف في مجلد «نزّل الفيديو»");
        OutputStream os = getContentResolver().openOutputStream(uri);
        FileInputStream fis = new FileInputStream(src);
        byte[] buf = new byte[1 << 16];
        int n;
        while ((n = fis.read(buf)) > 0) os.write(buf, 0, n);
        fis.close();
        os.close();
    }

    private static void cleanup(File... files) {
        for (File f : files) {
            if (f != null && f.exists()) f.delete();
        }
    }

    // ------------------------------------------------------------------ notifications

    private Notification build(String text, int pct, boolean indeterminate, boolean done) {
        Notification.Builder b = new Notification.Builder(this, CH)
                .setSmallIcon(done ? android.R.drawable.stat_sys_download_done
                        : android.R.drawable.stat_sys_download)
                .setContentTitle("نزّل الفيديو")
                .setContentText(text)
                .setOnlyAlertOnce(true);
        if (done) {
            b.setOngoing(false).setAutoCancel(true);
        } else {
            b.setOngoing(true).setProgress(indeterminate ? 0 : 100, indeterminate ? 0 : pct, indeterminate);
        }
        return b.build();
    }

    private Notification buildDone(String text) {
        return build(text, 0, false, true);
    }

    private static void notify(NotificationManager nm, int id, Notification n) {
        try {
            nm.notify(id, n);
        } catch (Exception ignored) {
        }
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }
}
