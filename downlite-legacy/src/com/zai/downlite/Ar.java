package com.zai.downlite;

/**
 * Arabic numerals & units for a 100% Arabic UI: every user-visible number renders
 * with Eastern Arabic-Indic digits (٠١٢٣٤٥٦٧٨٩) and Arabic unit names (م.ب / ك.ب /
 * ج.ب). Also used inside downloaded file names, so no Latin characters leak there.
 */
public final class Ar {

    private static final char[] D = {'٠', '١', '٢', '٣', '٤', '٥', '٦', '٧', '٨', '٩'};

    private Ar() {
    }

    /** Translate every Latin digit of s to its Eastern Arabic counterpart. */
    public static String n(String s) {
        if (s == null) return "";
        StringBuilder b = new StringBuilder(s.length());
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            b.append(c >= '0' && c <= '9' ? D[c - '0'] : c);
        }
        return b.toString();
    }

    public static String n(long v) {
        return n(Long.toString(v));
    }

    /** Human size: ٢٥٠٫٧ م.ب / ٧٣٥ ك.ب / ١٫٢ ج.ب (decimal MB, one decimal place). */
    public static String size(long bytes) {
        if (bytes <= 0) return "";
        if (bytes < 1024 * 1024) return n(Math.max(1, Math.round(bytes / 1024.0))) + " ك.ب";
        if (bytes < 1024L * 1024 * 1024) return n(String.format("%.1f", bytes / 1048576.0)) + " م.ب";
        return n(String.format("%.1f", bytes / 1073741824.0)) + " ج.ب";
    }

    /** Arabic count: ٠ ملفات، ملف واحد، ملفان، ٥ ملفات، ١٢ ملفًا. */
    public static String count(int n) {
        if (n == 1) return "ملف واحد";
        if (n == 2) return "ملفان";
        if (n >= 3 && n <= 10) return n(n) + " ملفات";
        return n(n) + " ملفًا";
    }
}
