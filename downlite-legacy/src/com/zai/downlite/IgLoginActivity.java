package com.zai.downlite;

import android.annotation.SuppressLint;
import android.app.Activity;
import android.graphics.Color;
import android.graphics.Typeface;
import android.graphics.drawable.GradientDrawable;
import android.os.Bundle;
import android.os.Handler;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.webkit.CookieManager;
import android.webkit.WebResourceRequest;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.LinearLayout;
import android.widget.TextView;
import android.widget.Toast;

/**
 * One-time Instagram login inside حمّل (v1.9): the official instagram.com login page
 * in a WebView. When the session cookie appears, it is captured, stored locally
 * (IgSession) and the activity returns RESULT_OK so the caller can auto-retry the
 * failed download. The phone's Instagram app session is never touched and never read;
 * everything stays in حمّل's private storage.
 */
public class IgLoginActivity extends Activity {

    private static final int BG = 0xFF0D141C;
    private static final int BG_ROW = 0xFF17222E;
    private static final int STROKE = 0xFF2A3644;
    private static final int TEXT_MAIN = 0xFFE8EEF5;
    private static final int TEXT_SUB = 0xFF9FB0C0;
    private static final int ACCENT = 0xFF00D0B0;

    private WebView web;
    private LinearLayout stateBox;
    private final Handler poll = new Handler();

    private int dp(float v) {
        return Math.round(getResources().getDisplayMetrics().density * v);
    }

    private GradientDrawable rounded(int color, float radius, int strokeColor, float strokeW) {
        GradientDrawable g = new GradientDrawable();
        g.setColor(color);
        g.setCornerRadius(dp(radius));
        if (strokeColor != 0) g.setStroke(Math.max(1, dp(strokeW)), strokeColor);
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

    private TextView button(String s, boolean accent) {
        TextView t = txt(s, 15, accent ? ACCENT : TEXT_MAIN, true);
        t.setGravity(Gravity.CENTER);
        t.setBackground(rounded(BG_ROW, 12, accent ? ACCENT : STROKE, 1));
        t.setPadding(dp(14), dp(12), dp(14), dp(12));
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.topMargin = dp(8);
        t.setLayoutParams(lp);
        return t;
    }

    @SuppressLint("SetJavaScriptEnabled")
    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        IgSession.init(this);
        try {
            LinearLayout root = new LinearLayout(this);
            root.setOrientation(LinearLayout.VERTICAL);
            root.setBackgroundColor(BG);
            root.setPadding(dp(16), dp(12), dp(16), dp(12));
            root.setLayoutDirection(View.LAYOUT_DIRECTION_RTL);

            // header: title + close
            LinearLayout head = new LinearLayout(this);
            head.setOrientation(LinearLayout.HORIZONTAL);
            head.setGravity(Gravity.CENTER_VERTICAL);
            TextView title = txt("تسجيل الدخول إلى إنستغرام", 17, TEXT_MAIN, true);
            TextView close = txt("✕", 18, TEXT_SUB, false);
            close.setPadding(dp(12), dp(4), dp(4), dp(4));
            close.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) {
                    finish();
                }
            });
            head.addView(title, new LinearLayout.LayoutParams(0,
                    ViewGroup.LayoutParams.WRAP_CONTENT, 1));
            head.addView(close);
            root.addView(head);

            TextView sub = txt("سجّل الدخول مرة واحدة فقط — تُحفظ الجلسة على هاتفك داخل «نزّل الفيديو» "
                    + "ولا تُرسل لأي جهة أخرى، ويمكن حذفها في أي وقت", 12, TEXT_SUB, false);
            sub.setPadding(dp(2), dp(6), dp(2), dp(10));
            root.addView(sub);

            stateBox = new LinearLayout(this);
            stateBox.setOrientation(LinearLayout.VERTICAL);
            root.addView(stateBox, new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));

            // the login WebView itself
            web = new WebView(this);
            web.getSettings().setJavaScriptEnabled(true);
            web.getSettings().setDomStorageEnabled(true);
            web.setVisibility(View.GONE);
            web.setWebViewClient(new WebViewClient() {
                @Override
                public boolean shouldOverrideUrlLoading(WebView w, WebResourceRequest r) {
                    try {
                        String u = r.getUrl().toString();
                        return !u.contains("instagram.com"); // keep other domains outside
                    } catch (Throwable t) {
                        return false;
                    }
                }

                @Override
                public void onPageFinished(WebView w, String url) {
                    checkCookies();
                }
            });
            root.addView(web, new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, 0, 1));

            setContentView(root);
            showState();
        } catch (Throwable t) {
            try {
                Toast.makeText(this, "تعذر فتح صفحة الدخول — تحقق من الإنترنت", Toast.LENGTH_LONG).show();
            } catch (Throwable ignored) {
            }
            finish();
        }
    }

    /** Idle screen: current session state + actions. The WebView opens on demand. */
    private void showState() {
        try {
            stateBox.removeAllViews();
            boolean logged = IgSession.has();
            TextView state = txt(logged
                    ? "✅ حساب إنستغرام متصل داخل نزّل الفيديو — التحميل من إنستغرام يعمل بجلستك"
                    : "ℹ️ إنستغرام غير متصل داخل نزّل الفيديو — بعض المنشورات ترفض التحليل بدون تسجيل دخول",
                    13, logged ? ACCENT : TEXT_SUB, false);
            state.setPadding(dp(4), dp(2), dp(4), dp(4));
            stateBox.addView(state);

            TextView open = button(logged ? "🔄 تحديث الجلسة (تسجيل دخول جديد)" : "🔑 تسجيل الدخول الآن",
                    true);
            open.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) {
                    openLogin();
                }
            });
            stateBox.addView(open);

            if (logged) {
                TextView out = button("🗑 حذف الجلسة المحفوظة", false);
                out.setOnClickListener(new View.OnClickListener() {
                    public void onClick(View v) {
                        IgSession.clear();
                        showState();
                    }
                });
                stateBox.addView(out);
            }

            TextView privacy = txt("تنبيه: استخدم حسابك الشخصي فقط. الجلسة تُستخدم فقط لجلب "
                    + "بيانات المنشورات عند التحميل وتُخزَّن في مساحة نزّل الفيديو الخاصة.",
                    11, TEXT_SUB, false);
            privacy.setPadding(dp(4), dp(8), dp(4), dp(4));
            stateBox.addView(privacy);
        } catch (Throwable ignored) {
        }
    }

    @SuppressLint("SetJavaScriptEnabled")
    private void openLogin() {
        try {
            stateBox.removeAllViews();
            TextView hint = txt("جارٍ فتح صفحة إنستغرام الرسمية…", 13, TEXT_SUB, false);
            hint.setPadding(dp(4), dp(4), dp(4), dp(4));
            stateBox.addView(hint);
            web.setVisibility(View.VISIBLE);
            // start at the login page; after success Instagram lands on the home feed
            web.loadUrl("https://www.instagram.com/accounts/login/");
            poll.removeCallbacks(checker);
            poll.postDelayed(checker, 1200);
        } catch (Throwable t) {
            try {
                Toast.makeText(this, "تعذر فتح الصفحة — تحقق من الإنترنت", Toast.LENGTH_LONG).show();
            } catch (Throwable ignored) {
            }
        }
    }

    /** Poll for the session cookie (login completes via XHR, not always a page load). */
    private final Runnable checker = new Runnable() {
        public void run() {
            try {
                if (web.getVisibility() != View.VISIBLE) return;
                if (checkCookies()) return;
                poll.postDelayed(this, 1000);
            } catch (Throwable ignored) {
            }
        }
    };

    /** True when a sessionid was captured and saved. */
    private boolean checkCookies() {
        try {
            if (web.getVisibility() != View.VISIBLE) return false;
            String ck = CookieManager.getInstance().getCookie("https://www.instagram.com");
            if (ck != null && ck.contains("sessionid=")) {
                IgSession.save(this, ck);
                poll.removeCallbacks(checker);
                web.stopLoading();
                web.setVisibility(View.GONE);
                Toast.makeText(this, "تم تسجيل الدخول ✓ — جارٍ إعادة المحاولة…",
                        Toast.LENGTH_SHORT).show();
                setResult(RESULT_OK);
                finish();
                return true;
            }
        } catch (Throwable ignored) {
        }
        return false;
    }

    @Override
    protected void onDestroy() {
        poll.removeCallbacks(checker);
        try {
            if (web != null) web.destroy();
        } catch (Throwable ignored) {
        }
        super.onDestroy();
    }
}
