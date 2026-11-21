package com.zai.downlite;

import android.app.Activity;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.graphics.Typeface;
import android.graphics.drawable.GradientDrawable;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.widget.LinearLayout;
import android.widget.ProgressBar;
import android.widget.TextView;
import android.widget.Toast;

import org.json.JSONObject;

import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * Dialog-styled translucent activity: sharing a YouTube link pops the quality sheet
 * straight on top of the sharing app (no app UI opens). Picking a quality hands the
 * job to DownloadService and closes immediately.
 *
 * Robustness rules (v1.5):
 *  - The sheet NEVER closes on outside touches (explicitly disabled) and every UI
 *    mutation is crash-guarded, so no path can silently exit.
 *  - While parsing, a live ticker shows elapsed seconds; after 20s a retry row
 *    appears even though parsing may still land late (stale results are dropped
 *    via a generation counter).
 *  - Quality rows live in a proper MATCH_PARENT container (v1.4 bug: zero-sized
 *    container made every row invisible → "connecting… then nothing happens").
 */
public class QuickDownloadActivity extends Activity {

    private static final Pattern URL_IN_TEXT = Pattern.compile("https?://\\S+");
    private static final Pattern YT = Pattern.compile("(?i)(youtube\\.com|youtu\\.be)");
    private static final Pattern IG = Pattern.compile("(?i)(instagram\\.com|instagr\\.am)");

    private static final int BG_CARD = 0xFF111A24;
    private static final int BG_ROW = 0xFF1A2532;
    private static final int STROKE = 0xFF2A3644;
    private static final int TEXT_MAIN = 0xFFE8EEF5;
    private static final int TEXT_SUB = 0xFF9FB0C0;
    private static final int ACCENT = 0xFF00D0B0;

    private LinearLayout rowsBox, loadRow;
    private TextView headTitle, headSub;
    private String videoTitle = "فيديو";
    private String pendingUrl;
    private String pendingVid = "";

    private final Handler ui = new Handler();
    private volatile int generation = 0;
    private volatile boolean awaiting = false;
    private long startMs;
    private boolean slowShown = false;
    private String[] pendingJob;

    private int dp(float v) {
        return Math.round(getResources().getDisplayMetrics().density * v);
    }

    private GradientDrawable rounded(int color, float radius, int strokeColor, float strokeW) {
        GradientDrawable g = new GradientDrawable();
        g.setColor(color);
        g.setCornerRadius(dp(radius));
        if (strokeColor != 0) {
            g.setStroke(Math.max(1, dp(strokeW)), strokeColor);
        }
        return g;
    }

    private TextView txt(String s, float sizeSp, int color, boolean bold) {
        TextView t = new TextView(this);
        t.setText(s);
        t.setTextSize(sizeSp);
        t.setTextColor(color);
        t.setTypeface(Typeface.create("sans-serif", bold ? Typeface.BOLD : Typeface.NORMAL));
        return t;
    }

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        IgSession.init(this);
        try {
            setFinishOnTouchOutside(false); // never exit silently — only ✕ / back / a choice
            getWindow().setGravity(Gravity.BOTTOM);

            String shared = null;
            if (Intent.ACTION_SEND.equals(getIntent().getAction())) {
                shared = getIntent().getStringExtra(Intent.EXTRA_TEXT);
            } else if (Intent.ACTION_VIEW.equals(getIntent().getAction())) {
                shared = getIntent().getDataString();
            }
            String url = null;
            if (shared != null) {
                Matcher m = URL_IN_TEXT.matcher(shared);
                url = m.find() ? m.group(0) : shared.trim();
            }
            final String furl = url;
            pendingUrl = url;

            LinearLayout card = new LinearLayout(this);
            card.setOrientation(LinearLayout.VERTICAL);
            card.setBackground(rounded(BG_CARD, 22, STROKE, 1));
            card.setPadding(dp(18), dp(16), dp(18), dp(16));
            card.setLayoutDirection(View.LAYOUT_DIRECTION_RTL);

            // header: title + close
            LinearLayout head = new LinearLayout(this);
            head.setOrientation(LinearLayout.HORIZONTAL);
            head.setGravity(Gravity.CENTER_VERTICAL);
            headTitle = txt("تنزيل الفيديو", 17, TEXT_MAIN, true);
            headTitle.setPadding(dp(6), 0, 0, 0);
            TextView close = txt("✕", 18, TEXT_SUB, false);
            close.setPadding(dp(10), dp(2), dp(2), dp(2));
            close.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) {
                    finish();
                }
            });
            head.addView(headTitle, new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1));
            head.addView(close);
            card.addView(head);

            headSub = txt("جارٍ التحليل…", 13, TEXT_SUB, false);
            headSub.setPadding(dp(6), dp(4), 0, dp(8));
            card.addView(headSub);

            // loading row: spinner + static label, hidden once results/errors arrive
            loadRow = new LinearLayout(this);
            loadRow.setOrientation(LinearLayout.HORIZONTAL);
            loadRow.setGravity(Gravity.CENTER_VERTICAL);
            ProgressBar pb = new ProgressBar(this);
            pb.setLayoutParams(new LinearLayout.LayoutParams(dp(22), dp(22)));
            loadRow.addView(pb);
            TextView loadTxt = txt(" الاتصال بمحرك التنزيل…", 14, TEXT_MAIN, false);
            loadRow.addView(loadTxt);
            card.addView(loadRow);

            // results container — real size, starts hidden (v1.4 bug fix)
            rowsBox = new LinearLayout(this);
            rowsBox.setOrientation(LinearLayout.VERTICAL);
            rowsBox.setVisibility(View.GONE);
            card.addView(rowsBox, new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));

            setContentView(card, new ViewGroup.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT,
                    ViewGroup.LayoutParams.WRAP_CONTENT));

            if (furl == null) {
                finish();
                return;
            }

            if (YT.matcher(furl).find() || IG.matcher(furl).find()) {
                // engine links (YouTube via NewPipe, Instagram via InstagramExtractor):
                // show the quality sheet instead of downloading the web page itself
                startExtraction();
            } else {
                // any other link: one-tap direct download of the file the URL points to
                directDownload(furl);
            }
        } catch (Throwable t) {
            // the sheet must never die silently — if anything above fails, say so and close
            try {
                Toast.makeText(this, "تعذر فتح لوحة التنزيل — أعد المشاركة", Toast.LENGTH_LONG).show();
            } catch (Throwable ignored) {
            }
            finish();
        }
    }

    @Override
    protected void onDestroy() {
        awaiting = false;
        ui.removeCallbacks(ticker);
        super.onDestroy();
    }

    // ------------------------------------------------------------------ extraction flow

    private final Runnable ticker = new Runnable() {
        public void run() {
            if (!awaiting) return;
            try {
                long s = (System.currentTimeMillis() - startMs) / 1000;
                headSub.setText("جارٍ التحليل… " + Ar.n(s) + " ث");
                if (s >= 20 && !slowShown) {
                    slowShown = true;
                    showFailure("الاتصال بطيء — تحقق من اتصال الإنترنت أو أعد المحاولة");
                    return; // parsing thread may still land later and replace this
                }
            } catch (Throwable ignored) {
                return;
            }
            ui.postDelayed(this, 1000);
        }
    };

    private void startExtraction() {
        awaiting = true;
        slowShown = false;
        startMs = System.currentTimeMillis();
        final int gen = ++generation;
        loadRow.setVisibility(View.VISIBLE);
        rowsBox.setVisibility(View.GONE);
        rowsBox.removeAllViews();
        headSub.setText("جارٍ التحليل…");
        ui.removeCallbacks(ticker);
        ui.postDelayed(ticker, 1000);
        new Thread(new Runnable() {
            public void run() {
                JSONObject out;
                try {
                    out = Extractor.extract(pendingUrl);
                } catch (Throwable t) { // extract() swallows everything — belt and braces
                    out = null;
                }
                final JSONObject fout = out;
                ui.post(new Runnable() {
                    public void run() {
                        if (gen != generation) return; // stale result after a retry
                        awaiting = false;
                        ui.removeCallbacks(ticker);
                        populate(fout);
                    }
                });
            }
        }).start();
    }

    /** One-tap direct download for non-YouTube URLs, shown inside the same floating sheet. */
    private void directDownload(String url) {
        loadRow.setVisibility(View.GONE);
        rowsBox.setVisibility(View.VISIBLE);
        rowsBox.removeAllViews();
        headTitle.setText("رابط مباشر");
        headSub.setText("اضغط للتحميل الآن — يُحفظ في مجلد «نزّل الفيديو»");
        String name = url.substring(url.lastIndexOf('/') + 1);
        int q = name.indexOf('?');
        if (q >= 0) name = name.substring(0, q);
        try {
            name = java.net.URLDecoder.decode(name, "UTF-8");
        } catch (Exception ignored) {
        }
        if (name.length() > 60 || name.isEmpty() || !name.contains(".")) {
            name = "download_" + System.currentTimeMillis() % 100000 + ".mp4";
        }
        final String fname = Extractor.sanitize(name);
        final String mime = mimeFor(fname);
        TextView row = rowButton("⬇️  تحميل الآن", true);
        row.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                beginDownload(null, pendingUrl, null, fname, mime);
            }
        });
        rowsBox.addView(row);
    }

    private static String mimeFor(String name) {
        String n = name.toLowerCase();
        if (n.endsWith(".mp4") || n.endsWith(".m4v") || n.endsWith(".mov")) return "video/mp4";
        if (n.endsWith(".webm")) return "video/webm";
        if (n.endsWith(".mp3")) return "audio/mpeg";
        if (n.endsWith(".m4a") || n.endsWith(".aac")) return "audio/mp4";
        if (n.endsWith(".pdf")) return "application/pdf";
        if (n.endsWith(".jpg") || n.endsWith(".jpeg")) return "image/jpeg";
        if (n.endsWith(".png")) return "image/png";
        if (n.endsWith(".gif")) return "image/gif";
        return "application/octet-stream";
    }

    // ------------------------------------------------------------------ results / errors

    private void populate(JSONObject out) {
        try {
            populateInner(out);
        } catch (Throwable t) { // a broken result must never kill the process
            showFailure("حدث خطأ غير متوقع — أعد المحاولة");
        }
    }

    private void populateInner(JSONObject out) {
        loadRow.setVisibility(View.GONE);
        rowsBox.setVisibility(View.VISIBLE);
        rowsBox.removeAllViews();
        if (out == null || !out.optBoolean("ok", false)) {
            String msg = out == null || !out.has("error") ? "فشل التحليل"
                    : friendly(out.optString("error", "فشل التحليل"));
            boolean loginFixes = out != null && out.optBoolean("needsLogin", false);
            showFailure(msg, loginFixes);
            return;
        }
        videoTitle = out.optString("title", "فيديو");
        pendingVid = out.optString("vid", "");
        headTitle.setText(videoTitle.length() > 42 ? videoTitle.substring(0, 42) + "…" : videoTitle);
        int dur = (int) out.optLong("duration", 0);
        String sub = out.optString("uploader", "");
        if (dur > 0) sub += (sub.isEmpty() ? "" : " • ") + Ar.n(Math.max(1, dur / 60)) + " دقيقة";
        headSub.setText(sub.isEmpty() ? "اختر الجودة — التنزيل يبدأ فورًا" : sub + " — اختر الجودة");

        org.json.JSONArray formats = out.optJSONArray("formats");
        if (formats == null || formats.length() == 0) {
            headSub.setText("لا توجد صيغ متاحة لهذا الفيديو");
            return;
        }
        for (int i = 0; i < formats.length(); i++) {
            final JSONObject f = formats.optJSONObject(i);
            if (f == null) continue;
            boolean merge = f.optBoolean("merge", false);
            String label = f.optString("label", "");
            long size = f.optLong("size", 0);
            String kind = f.optString("kind");
            String icon = "audio".equals(kind) ? "🎵 " : ("image".equals(kind) ? "🖼️ " : "🎬 ");
            String t = icon + label + (size > 0 ? " — " + Ar.size(size) : "");
            TextView row = rowButton(t, merge);
            row.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) {
                    String m = f.optString("mime", "video/mp4");
                    String ext = "audio/mp4".equals(m) ? ".m4a"
                            : ("audio/mpeg".equals(m) ? ".mp3"
                            : ("image/jpeg".equals(m) ? ".jpg"
                            : ("image/png".equals(m) ? ".png" : ".mp4")));
                    String name = Extractor.sanitize(videoTitle) + " [" + f.optString("label", "فيديو") + "]" + ext;
                    beginDownload(pendingVid,
                            f.optString("url", ""), f.optString("aurl", null), name,
                            f.optString("mime", "video/mp4"));
                }
            });
            rowsBox.addView(row);
        }
        TextView foot = txt("يُحفظ الملف في مجلد «نزّل الفيديو» داخل التنزيلات", 11, TEXT_SUB, false);
        foot.setPadding(dp(6), dp(10), 0, 0);
        rowsBox.addView(foot);
    }

    private void showFailure(String msg) {
        showFailure(msg, false);
    }

    /** Visible Arabic failure, never a silent exit. When one in-app Instagram login
     *  fixes it (needsLogin), a dedicated login button sits above the retry row. */
    private void showFailure(String msg, boolean needsLogin) {
        loadRow.setVisibility(View.GONE);
        rowsBox.setVisibility(View.VISIBLE);
        rowsBox.removeAllViews();
        headSub.setText(msg);
        if (needsLogin) {
            TextView login = rowButton("🔑 دخول اختياري بجلستك داخل التطبيق — يحل المشكلة نهائيًا", true);
            login.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) {
                    try {
                        Intent i = new Intent(QuickDownloadActivity.this, IgLoginActivity.class);
                        startActivityForResult(i, 42);
                    } catch (Throwable t) {
                        Toast.makeText(QuickDownloadActivity.this,
                                "تعذر فتح صفحة الدخول", Toast.LENGTH_SHORT).show();
                    }
                }
            });
            rowsBox.addView(login);
        }
        TextView retry = rowButton("🔄 إعادة المحاولة", false);
        retry.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                if (pendingUrl != null) startExtraction();
            }
        });
        rowsBox.addView(retry);
    }

    /** Login done inside the app → immediately retry the extraction that failed before. */
    @Override
    protected void onActivityResult(int requestCode, int resultCode, Intent data) {
        super.onActivityResult(requestCode, resultCode, data);
        if (requestCode == 42 && resultCode == RESULT_OK && pendingUrl != null) {
            startExtraction();
        }
    }

    private String friendly(String msg) {
        if (msg == null) return "فشل التحليل";
        String m = msg.toLowerCase();
        if (m.matches("(?i).*(not a bot|login_required|signinconfirm).*")) {
            return "يوتيوب رفض الطلب مؤقتًا — أعد المحاولة بعد قليل";
        }
        if (m.matches(".*(timeout|unknownhost|connection|network|socket|resolve).*")) {
            return "تعذر الاتصال بالشبكة — تحقق من الإنترنت وأعد المحاولة";
        }
        return msg.length() > 90 ? msg.substring(0, 90) + "…" : msg;
    }

    private TextView rowButton(String text, boolean highlight) {
        TextView t = new TextView(this);
        t.setText(text);
        t.setTextSize(15);
        t.setTextColor(highlight ? ACCENT : TEXT_MAIN);
        t.setTypeface(Typeface.create("sans-serif", Typeface.BOLD));
        t.setBackground(rounded(BG_ROW, 12, STROKE, 1));
        t.setPadding(dp(14), dp(13), dp(14), dp(13));
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.topMargin = dp(6);
        t.setLayoutParams(lp);
        return t;
    }

    // ------------------------------------------------------------------ hand-off to the service

    /** Ask (once, ever) for POST_NOTIFICATIONS so progress is visible on Android 13+,
     *  then start the download regardless of the answer. */
    private void beginDownload(String vid, String vUrl, String aUrl, String name, String mime) {
        boolean granted = true;
        if (Build.VERSION.SDK_INT >= 33) {
            try {
                granted = checkSelfPermission(android.Manifest.permission.POST_NOTIFICATIONS)
                        == PackageManager.PERMISSION_GRANTED;
            } catch (Throwable ignored) {
            }
        }
        if (!granted && !getSharedPreferences("downlite", MODE_PRIVATE)
                .getBoolean("notif_asked", false)) {
            getSharedPreferences("downlite", MODE_PRIVATE).edit().putBoolean("notif_asked", true).apply();
            pendingJob = new String[]{vid == null ? "" : vid, vUrl, aUrl == null ? "" : aUrl, name, mime};
            try {
                requestPermissions(new String[]{android.Manifest.permission.POST_NOTIFICATIONS}, 7);
                return;
            } catch (Throwable ignored) {
                pendingJob = null;
            }
        }
        launchDownload(vid, vUrl, aUrl, name, mime);
    }

    @Override
    public void onRequestPermissionsResult(int code, String[] perms, int[] results) {
        super.onRequestPermissionsResult(code, perms, results);
        if (code == 7 && pendingJob != null) {
            String[] j = pendingJob;
            pendingJob = null;
            launchDownload(j[0].isEmpty() ? null : j[0], j[1], j[2].isEmpty() ? null : j[2], j[3], j[4]);
        }
    }

    private void launchDownload(String vid, String vUrl, String aUrl, String name, String mime) {
        try {
            DownloadService.start(QuickDownloadActivity.this, vid, vUrl, aUrl, name, mime);
            Toast.makeText(QuickDownloadActivity.this,
                    "بدأ التنزيل في الخلفية — تابع التقدم من الإشعارات", Toast.LENGTH_SHORT).show();
        } catch (Throwable t) {
            Toast.makeText(QuickDownloadActivity.this,
                    "تعذر بدء التنزيل — أعد المحاولة", Toast.LENGTH_LONG).show();
        }
        finish();
    }
}
