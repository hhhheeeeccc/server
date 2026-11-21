package com.zai.downlite;

import android.app.Activity;
import android.app.AlertDialog;
import android.content.ContentValues;
import android.content.DialogInterface;
import android.content.Intent;
import android.database.Cursor;
import android.graphics.Color;
import android.graphics.Typeface;
import android.graphics.drawable.GradientDrawable;
import android.net.Uri;
import android.os.Bundle;
import android.provider.MediaStore;
import android.text.InputType;
import android.text.TextUtils;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.widget.EditText;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.TextView;
import android.widget.Toast;

import java.util.Calendar;

/**
 * The whole app face (v1.7 redesign, Arabic-only, RTL): header with the app name,
 * cards for every file the app downloaded (MediaStore Downloads, folders «نزّل الفيديو»,
 * «حمّل» and the legacy DownLite), each with icon, Arabic size and Arabic date.
 * Tap a card to open the file; long-press it to rename or delete it (v2.1).
 */
public class MainActivity extends Activity {

    private static final int BG = 0xFF0D141C;
    private static final int BG_ROW = 0xFF17222E;
    private static final int STROKE = 0xFF2A3644;
    private static final int TEXT_MAIN = 0xFFE8EEF5;
    private static final int TEXT_SUB = 0xFF9FB0C0;
    private static final int ACCENT = 0xFF00D0B0;

    private LinearLayout listBox;
    private TextView igCard;

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

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        IgSession.init(this);

        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setBackgroundColor(BG);
        root.setPadding(dp(16), dp(14), dp(16), dp(14));
        root.setLayoutDirection(View.LAYOUT_DIRECTION_RTL);

        // header: app name + tagline
        LinearLayout head = new LinearLayout(this);
        head.setOrientation(LinearLayout.HORIZONTAL);
        head.setGravity(Gravity.CENTER_VERTICAL);
        TextView logo = txt("نزّل الفيديو", 21, ACCENT, true);
        TextView tag = txt("  —  بسرعة وبدون إعلانات", 12, TEXT_SUB, false);
        head.addView(logo);
        head.addView(tag);
        root.addView(head);

        TextView sub = txt("ملفاتك المحمّلة تظهر هنا", 12, TEXT_SUB, false);
        sub.setPadding(0, dp(2), 0, dp(12));
        root.addView(sub);

        // Instagram session card: tap to log in inside حمّل (fixes «إنستغرام يطلب تسجيل دخول»)
        LinearLayout ig = new LinearLayout(this);
        ig.setOrientation(LinearLayout.HORIZONTAL);
        ig.setGravity(Gravity.CENTER_VERTICAL);
        ig.setBackground(rounded(BG_ROW, 14, STROKE, 1));
        ig.setPadding(dp(14), dp(11), dp(14), dp(11));
        ig.setLayoutDirection(View.LAYOUT_DIRECTION_RTL);
        igCard = txt("", 13, TEXT_MAIN, true);
        igCard.setMaxLines(3);
        ig.addView(igCard, new LinearLayout.LayoutParams(0,
                ViewGroup.LayoutParams.WRAP_CONTENT, 1));
        ig.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                try {
                    startActivityForResult(new Intent(MainActivity.this, IgLoginActivity.class), 5);
                } catch (Exception ignored) {
                }
            }
        });
        root.addView(ig, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));
        ((LinearLayout.LayoutParams) ig.getLayoutParams()).bottomMargin = dp(10);
        refreshIgCard();

        ScrollView sc = new ScrollView(this);
        listBox = new LinearLayout(this);
        listBox.setOrientation(LinearLayout.VERTICAL);
        sc.addView(listBox);
        root.addView(sc, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT, 1));

        setContentView(root);
        refresh();
    }

    @Override
    protected void onResume() {
        super.onResume();
        refresh();
        refreshIgCard();
    }

    /** Live Instagram-session state under the header: connected / not connected. */
    private void refreshIgCard() {
        if (igCard == null) return;
        try {
            if (IgSession.has()) {
                igCard.setText("✅  حساب إنستغرام متصل — التحميل بجلستك");
                igCard.setTextColor(ACCENT);
            } else {
                igCard.setText("ℹ️  المنشورات العامة تُحمَّل بدون أي تسجيل دخول — اضغط هنا فقط إذا طلب إنستغرام الدخول");
                igCard.setTextColor(TEXT_MAIN);
            }
        } catch (Throwable ignored) {
        }
    }

    private void refresh() {
        listBox.removeAllViews();
        int n = 0;
        try {
            // all folders: the current «نزّل الفيديو» plus the older «حمّل» and DownLite
            Cursor c = getContentResolver().query(
                    MediaStore.Downloads.EXTERNAL_CONTENT_URI,
                    new String[]{MediaStore.MediaColumns._ID,
                            MediaStore.MediaColumns.DISPLAY_NAME,
                            MediaStore.MediaColumns.MIME_TYPE,
                            MediaStore.MediaColumns.SIZE,
                            MediaStore.MediaColumns.DATE_ADDED},
                    MediaStore.MediaColumns.RELATIVE_PATH + " LIKE ? OR "
                            + MediaStore.MediaColumns.RELATIVE_PATH + " LIKE ? OR "
                            + MediaStore.MediaColumns.RELATIVE_PATH + " LIKE ?",
                    new String[]{"Download/نزّل الفيديو%", "Download/حمّل%", "Download/DownLite%"},
                    MediaStore.MediaColumns.DATE_ADDED + " DESC");
            if (c != null) {
                while (c.moveToNext()) {
                    n++;
                    listBox.addView(row(c.getLong(0), c.getString(1), c.getString(2),
                            c.getLong(3), c.getLong(4)));
                }
                c.close();
            }
        } catch (Exception ignored) {
        }
        if (n == 0) {
            TextView empty = txt("لا توجد ملفات بعد\n\nشارك أي فيديو من يوتيوب أو إنستغرام\nواختر «نزّل الفيديو» — ستظهر الجودات فورًا،\nاختر الجودة أو الصوت ويبدأ التنزيل مباشرة\n\nاضغط مطولًا على أي ملف لاحقًا: تعديل الاسم أو الحذف",
                    14, TEXT_SUB, false);
            empty.setPadding(dp(6), dp(30), dp(6), dp(6));
            empty.setGravity(Gravity.CENTER);
            empty.setLineSpacing(dp(4), 1);
            listBox.addView(empty);
        } else {
            TextView foot = txt(Ar.count(n) + " • اضغط مطولًا على أي ملف: تعديل الاسم أو الحذف", 12, TEXT_SUB, false);
            foot.setPadding(dp(6), dp(10), 0, dp(6));
            listBox.addView(foot);
        }
    }

    private static String iconFor(String mime) {
        String m = mime == null ? "" : mime;
        if (m.startsWith("video/")) return "🎬";
        if (m.startsWith("audio/")) return "🎵";
        if (m.startsWith("image/")) return "🖼️";
        return "📄";
    }

    /** Arabic date: «اليوم ١٤:٣٢» / «أمس» / «٢٠/١١/٢٠٢٦». */
    private static String dateAr(long epochSeconds) {
        if (epochSeconds <= 0) return "";
        Calendar now = Calendar.getInstance();
        Calendar d = Calendar.getInstance();
        d.setTimeInMillis(epochSeconds * 1000L);
        int dayDiff = now.get(Calendar.DAY_OF_YEAR) - d.get(Calendar.DAY_OF_YEAR)
                + (now.get(Calendar.YEAR) - d.get(Calendar.YEAR)) * 365;
        if (dayDiff == 0) {
            return "اليوم " + Ar.n(String.format("%02d:%02d",
                    d.get(Calendar.HOUR_OF_DAY), d.get(Calendar.MINUTE)));
        }
        if (dayDiff == 1) return "أمس";
        return Ar.n(String.format("%02d/%02d/%04d",
                d.get(Calendar.DAY_OF_MONTH), d.get(Calendar.MONTH) + 1, d.get(Calendar.YEAR)));
    }

    private View row(final long id, String name, String mime, long size, long added) {
        LinearLayout r = new LinearLayout(this);
        r.setOrientation(LinearLayout.VERTICAL);
        r.setBackground(rounded(BG_ROW, 14, STROKE, 1));
        r.setPadding(dp(14), dp(11), dp(14), dp(11));
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.topMargin = dp(7);
        r.setLayoutParams(lp);

        TextView t1 = txt(iconFor(mime) + "  " + name, 14, TEXT_MAIN, true);
        t1.setMaxLines(2);
        t1.setEllipsize(TextUtils.TruncateAt.END);
        r.addView(t1);

        String info = Ar.size(size);
        String when = dateAr(added);
        if (when.isEmpty()) {
            info = when.isEmpty() ? info : when;
        } else {
            info = info.isEmpty() ? when : info + " • " + when;
        }
        TextView t2 = txt(info, 11, TEXT_SUB, false);
        t2.setPadding(dp(30), dp(1), 0, 0);
        t2.setVisibility(info.isEmpty() ? View.GONE : View.VISIBLE);
        r.addView(t2);
        r.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                try {
                    Intent i = new Intent(Intent.ACTION_VIEW);
                    i.setDataAndType(Uri.parse("content://media/external/downloads/" + id),
                            mime == null ? "*/*" : mime);
                    i.addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION);
                    startActivity(i);
                } catch (Exception e) {
                    Toast.makeText(MainActivity.this, "لا يوجد تطبيق يفتح هذا الملف", Toast.LENGTH_SHORT).show();
                }
            }
        });
        r.setOnLongClickListener(new View.OnLongClickListener() {
            public boolean onLongClick(View v) {
                showManage(id, name == null ? "" : name);
                return true;
            }
        });
        return r;
    }

    // ------------------------------------------------------------------ rename / delete (v2.1)

    /** Long-press on a file card: rename or delete the file via MediaStore. */
    private void showManage(final long id, final String name) {
        CharSequence[] items = {"✏️  تعديل الاسم", "🗑  حذف من الهاتف"};
        new AlertDialog.Builder(this)
                .setTitle(name)
                .setItems(items, new DialogInterface.OnClickListener() {
                    public void onClick(DialogInterface d, int which) {
                        if (which == 0) showRename(id, name);
                        else confirmDelete(id, name);
                    }
                })
                .setNegativeButton("إلغاء", null)
                .show();
    }

    /** Rename dialog: the extension is kept automatically if the user omits it. */
    private void showRename(final long id, final String oldName) {
        final EditText edit = new EditText(this);
        edit.setText(oldName);
        edit.setTextSize(15);
        edit.setTextColor(TEXT_MAIN);
        edit.setHint("الاسم الجديد");
        edit.setInputType(InputType.TYPE_CLASS_TEXT);
        edit.setSingleLine(false);
        edit.setPadding(dp(14), dp(10), dp(14), dp(10));
        edit.setBackground(rounded(BG_ROW, 10, STROKE, 1));
        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(10), dp(4), dp(10), 0);
        box.addView(edit, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));
        TextView hint = txt("اترك الامتداد (.mp4 مثلًا) كما هو لتفتح الملف بسهولة", 11, TEXT_SUB, false);
        hint.setPadding(dp(14), dp(8), dp(14), dp(4));
        box.addView(hint);

        new AlertDialog.Builder(this)
                .setTitle("تعديل الاسم")
                .setView(box)
                .setPositiveButton("حفظ", new DialogInterface.OnClickListener() {
                    public void onClick(DialogInterface d, int w) {
                        renameFile(id, oldName, edit.getText().toString().trim());
                    }
                })
                .setNegativeButton("إلغاء", null)
                .show();
    }

    private void renameFile(long id, String oldName, String newName) {
        if (newName.isEmpty()) {
            Toast.makeText(this, "الاسم فارغ — اكتب اسمًا", Toast.LENGTH_SHORT).show();
            return;
        }
        if (newName.equals(oldName)) return;
        int dot = oldName.lastIndexOf('.');
        String ext = (dot > 0) ? oldName.substring(dot) : "";
        if (!ext.isEmpty() && !newName.toLowerCase().endsWith(ext.toLowerCase())) {
            newName = newName + ext;
        }
        try {
            ContentValues cv = new ContentValues();
            cv.put(MediaStore.MediaColumns.DISPLAY_NAME, newName);
            int n = getContentResolver().update(MediaStore.Downloads.EXTERNAL_CONTENT_URI, cv,
                    MediaStore.MediaColumns._ID + "=?", new String[]{String.valueOf(id)});
            Toast.makeText(this, n > 0 ? "✅ تم تعديل الاسم" : "تعذر تعديل الاسم", Toast.LENGTH_SHORT).show();
        } catch (Exception e) {
            Toast.makeText(this, "تعذر تعديل الاسم", Toast.LENGTH_SHORT).show();
        }
        refresh();
    }

    private void confirmDelete(final long id, final String name) {
        new AlertDialog.Builder(this)
                .setTitle("حذف الملف")
                .setMessage("سيُحذف «" + name + "» نهائيًا من هاتفك.")
                .setPositiveButton("حذف", new DialogInterface.OnClickListener() {
                    public void onClick(DialogInterface d, int w) {
                        deleteFile(id);
                    }
                })
                .setNegativeButton("إلغاء", null)
                .show();
    }

    private void deleteFile(long id) {
        try {
            int n = getContentResolver().delete(MediaStore.Downloads.EXTERNAL_CONTENT_URI,
                    MediaStore.MediaColumns._ID + "=?", new String[]{String.valueOf(id)});
            Toast.makeText(this, n > 0 ? "🗑 تم حذف الملف" : "تعذر الحذف", Toast.LENGTH_SHORT).show();
        } catch (Exception e) {
            Toast.makeText(this, "تعذر الحذف", Toast.LENGTH_SHORT).show();
        }
        refresh();
    }
}
