package com.zai.downlite;

import android.content.Context;

import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.nio.charset.Charset;

/**
 * Instagram session inside نزّل الفيديو (v1.9 fix for «أنا مسجّل دخولي لكن التحميل يفشل»):
 * the Instagram app's login is sandboxed per-app by Android and is NEVER shared
 * with other apps, so this app must hold its own session. The user logs in once via
 * IgLoginActivity (official instagram.com web page inside a WebView); the session
 * cookies are filtered to the few names Instagram needs and stored in the app's
 * private files dir only. Nothing is uploaded anywhere; a plain uninstall removes it.
 *
 * Pure static helper, init() from app components; never throws to callers.
 */
public final class IgSession {

    private static final String FILE = "ig_session.txt";
    /** Only these cookies travel with requests — never the whole web jar. */
    private static final String[] KEEP = {
            "sessionid", "ds_user_id", "csrftoken", "rur", "mid", "ig_did"
    };

    private static volatile Context app;
    private static volatile String cache;

    private IgSession() {
    }

    /** Anchor an application context once (called from activities / the service). */
    public static void init(Context c) {
        if (app == null) {
            try {
                app = c.getApplicationContext();
            } catch (Throwable ignored) {
            }
        }
    }

    /** Keep only the cookies Instagram actually needs, joined as a Cookie header. */
    static String filter(String cookieHeader) {
        if (cookieHeader == null) return "";
        StringBuilder b = new StringBuilder();
        for (String pair : cookieHeader.split(";")) {
            String p = pair.trim();
            int eq = p.indexOf('=');
            if (eq <= 0) continue;
            String name = p.substring(0, eq);
            for (String k : KEEP) {
                if (k.equals(name)) {
                    if (b.length() > 0) b.append("; ");
                    b.append(p);
                    break;
                }
            }
        }
        return b.toString();
    }

    /** Persist the filtered cookie header (IgLoginActivity after a successful login). */
    public static void save(Context c, String cookieHeader) {
        try {
            String f = filter(cookieHeader);
            if (f.isEmpty() || !f.contains("sessionid=")) return;
            FileOutputStream fo = c.getApplicationContext()
                    .openFileOutput(FILE, Context.MODE_PRIVATE);
            fo.write(f.getBytes(Charset.forName("UTF-8")));
            fo.close();
            cache = f;
        } catch (Throwable ignored) {
        }
    }

    /** Current cookie header or null when no session is stored. */
    public static String cookie() {
        if (cache != null) return cache;
        Context c = app;
        if (c == null) return null;
        try {
            File f = c.getFileStreamPath(FILE);
            if (!f.exists()) return null;
            FileInputStream fi = new FileInputStream(f);
            byte[] buf = new byte[(int) Math.min(f.length(), 65536)];
            int n = fi.read(buf);
            fi.close();
            String s = new String(buf, 0, Math.max(0, n), Charset.forName("UTF-8")).trim();
            cache = s.isEmpty() ? null : s;
        } catch (Throwable t) {
            cache = null;
        }
        return cache;
    }

    /** A session exists AND carries the login cookie that matters. */
    public static boolean has() {
        String s = cookie();
        return s != null && s.contains("sessionid=");
    }

    /** Forget the stored session (user logout from inside حمّل). */
    public static void clear() {
        try {
            Context c = app;
            if (c != null) c.getFileStreamPath(FILE).delete();
        } catch (Throwable ignored) {
        }
        cache = null;
    }
}
