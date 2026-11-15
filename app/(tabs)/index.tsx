/* eslint-disable react-hooks/set-state-in-effect -- mirrors an external Android share payload into the form */
import { useEffect, useMemo, useState } from "react";
import { ActivityIndicator, Alert, Linking, Platform, Share, StyleSheet, Text, TextInput, TouchableOpacity, View } from "react-native";

import { ScreenContainer } from "@/components/screen-container";
import { addSavedVideo } from "@/lib/download-history";
import { clearSharedPayloads, useSafeIncomingShare } from "@/lib/incoming-share";
import { downloadVideo, inspectVideoLink, saveToDeviceLibrary, type LinkInspection } from "@/lib/video";

const DARK = "#07111F";

export default function HomeScreen() {
  const [value, setValue] = useState("");
  const [inspection, setInspection] = useState<LinkInspection | null>(null);
  const [progress, setProgress] = useState(0);
  const [status, setStatus] = useState<"idle" | "downloading" | "success" | "error">("idle");
  const [error, setError] = useState("");
  const { resolvedSharedPayloads, refreshSharePayloads } = useSafeIncomingShare();

  useEffect(() => {
    const shared = resolvedSharedPayloads[0] as { value?: string; contentUri?: string | null } | undefined;
    if (!shared) return;
    const incoming = shared.value || shared.contentUri || "";
    if (incoming) {
      setValue(incoming);
      setInspection(inspectVideoLink(incoming));
      setStatus("idle");
    }
    clearSharedPayloads();
  }, [resolvedSharedPayloads]);

  const sourceLabel = useMemo(() => inspection?.source ?? "بانتظار رابط", [inspection]);

  const review = () => {
    const next = inspectVideoLink(value);
    setInspection(next);
    setStatus("idle");
    setError("");
  };

  const pasteFromShare = async () => {
    await refreshSharePayloads();
    if (Platform.OS === "web") {
      await Share.share({ message: "الصق الرابط في الحقل ثم اضغط فحص الرابط" });
    }
  };

  const startDownload = () => {
    if (!inspection?.canDownload) return;
    Alert.alert("تأكيد الحفظ", "أؤكد أن لدي حق استخدام هذا الفيديو وأن تنزيله مسموح به.", [
      { text: "إلغاء", style: "cancel" },
      { text: "أوافق وأحفظ", onPress: performDownload },
    ]);
  };

  const performDownload = async () => {
    if (!inspection?.canDownload) return;
    setStatus("downloading");
    setProgress(0);
    setError("");
    try {
      const downloaded = await downloadVideo(inspection.url, setProgress);
      let storedInLibrary = false;
      try { storedInLibrary = await saveToDeviceLibrary(downloaded.uri); } catch { storedInLibrary = false; }
      await addSavedVideo({
        id: `${Date.now()}`,
        filename: downloaded.filename,
        uri: downloaded.uri,
        source: inspection.source,
        originalUrl: inspection.url,
        createdAt: new Date().toISOString(),
      });
      setStatus("success");
      Alert.alert("تم الحفظ", storedInLibrary ? "تم حفظ الفيديو في مكتبة الجهاز وفي محفوظاتي." : "تم حفظ الفيديو داخل محفوظاتي في التطبيق.");
    } catch (downloadError) {
      setStatus("error");
      setError(downloadError instanceof Error ? downloadError.message : "حدث خطأ غير متوقع أثناء التنزيل");
    }
  };

  const openSource = async () => {
    if (!inspection?.url) return;
    try { await Linking.openURL(inspection.url); } catch { setError("تعذر فتح الرابط"); }
  };

  return (
    <ScreenContainer safeAreaClassName="bg-slate-950" className="bg-slate-950 px-5 pt-3">
      <View style={styles.topBar}>
        <View style={styles.brandMark}><Text style={styles.brandMarkText}>V</Text><Text style={styles.arrow}>↓</Text></View>
        <View style={styles.brandCopy}><Text style={styles.brand}>VideoLoad</Text><Text style={styles.tagline}>حفظ واضح. بدون تجاوز.</Text></View>
      </View>
      <View style={styles.hero}>
        <Text style={styles.kicker}>شارك الرابط إلى هنا</Text>
        <Text style={styles.title}>فيديوك في مكان واحد</Text>
        <Text style={styles.subtitle}>استقبل رابطًا من Snapchat أو Instagram أو Telegram أو YouTube، ثم افحصه واحفظ ما تملك حق استخدامه.</Text>
      </View>
      <View style={styles.inputCard}>
        <View style={styles.inputHeader}><Text style={styles.cardLabel}>رابط الفيديو</Text><Text style={styles.sourcePill}>{sourceLabel}</Text></View>
        <TextInput
          value={value}
          onChangeText={(text) => { setValue(text); setInspection(null); setStatus("idle"); }}
          placeholder="https://..."
          placeholderTextColor="#64748B"
          autoCapitalize="none"
          autoCorrect={false}
          keyboardType="url"
          textAlign="right"
          multiline
          style={styles.input}
        />
        <View style={styles.inputActions}>
          <TouchableOpacity onPress={pasteFromShare} style={styles.secondaryButton}><Text style={styles.secondaryText}>استقبال من المشاركة</Text></TouchableOpacity>
          <TouchableOpacity onPress={review} style={styles.primaryButton}><Text style={styles.primaryText}>فحص الرابط</Text></TouchableOpacity>
        </View>
      </View>
      {inspection && (
        <View style={[styles.statusCard, inspection.canDownload ? styles.goodCard : styles.warnCard]}>
          <View style={styles.statusRow}><Text style={styles.statusTitle}>{inspection.canDownload ? "الرابط جاهز للحفظ" : "راجع الرابط أولًا"}</Text><Text style={styles.statusDot}>{inspection.canDownload ? "●" : "!"}</Text></View>
          <Text style={styles.statusMessage}>{inspection.message}</Text>
          {inspection.url ? <Text numberOfLines={1} style={styles.urlPreview}>{inspection.url}</Text> : null}
          <View style={styles.statusActions}>
            <TouchableOpacity onPress={openSource} style={styles.linkButton}><Text style={styles.linkText}>فتح الرابط</Text></TouchableOpacity>
            {inspection.canDownload && <TouchableOpacity onPress={startDownload} disabled={status === "downloading"} style={styles.downloadButton}>{status === "downloading" ? <ActivityIndicator color={DARK} /> : <Text style={styles.downloadText}>حفظ الفيديو</Text>}</TouchableOpacity>}
          </View>
          {status === "downloading" && <View style={styles.progressWrap}><View style={[styles.progressBar, { width: `${Math.max(progress, 4)}%` }]} /><Text style={styles.progressText}>{progress}%</Text></View>}
          {status === "success" && <Text style={styles.successText}>تمت الإضافة إلى محفوظاتي.</Text>}
          {status === "error" && <Text style={styles.errorText}>{error}</Text>}
        </View>
      )}
      <View style={styles.note}><Text style={styles.noteIcon}>i</Text><Text style={styles.noteText}>لا يتجاوز VideoLoad حماية المنصات ولا ينزّل محتوى محميًا دون إذن. روابط صفحات المنصات قد تحتاج إلى رابط تنزيل مباشر مسموح.</Text></View>
    </ScreenContainer>
  );
}

const styles = StyleSheet.create({
  topBar: { flexDirection: "row-reverse", alignItems: "center", gap: 10, paddingTop: 10 },
  brandMark: { width: 44, height: 44, borderRadius: 15, backgroundColor: "#A7F3D0", alignItems: "center", justifyContent: "center", position: "relative" },
  brandMarkText: { color: DARK, fontSize: 24, fontWeight: "900", lineHeight: 26 },
  arrow: { color: "#0F766E", fontSize: 15, fontWeight: "900", position: "absolute", bottom: 2, right: 8 },
  brandCopy: { alignItems: "flex-end" },
  brand: { color: "#F8FAFC", fontSize: 18, fontWeight: "800" },
  tagline: { color: "#64748B", fontSize: 11, marginTop: 2 },
  hero: { alignItems: "flex-end", paddingTop: 34, paddingBottom: 22 },
  kicker: { color: "#A7F3D0", fontSize: 13, fontWeight: "800", marginBottom: 8 },
  title: { color: "#F8FAFC", fontSize: 32, lineHeight: 39, fontWeight: "900", textAlign: "right" },
  subtitle: { color: "#94A3B8", fontSize: 14, lineHeight: 23, textAlign: "right", marginTop: 9 },
  inputCard: { backgroundColor: "#111D2E", borderRadius: 23, borderWidth: 1, borderColor: "#22334A", padding: 16 },
  inputHeader: { flexDirection: "row-reverse", alignItems: "center", justifyContent: "space-between", marginBottom: 12 },
  cardLabel: { color: "#E2E8F0", fontSize: 14, fontWeight: "800" },
  sourcePill: { color: "#A7F3D0", backgroundColor: "#163B35", borderRadius: 9, paddingHorizontal: 9, paddingVertical: 5, fontSize: 11, fontWeight: "700" },
  input: { minHeight: 82, backgroundColor: "#07111F", borderRadius: 15, borderWidth: 1, borderColor: "#263B53", color: "#F8FAFC", paddingHorizontal: 14, paddingVertical: 13, fontSize: 14, textAlignVertical: "top" },
  inputActions: { flexDirection: "row-reverse", gap: 9, marginTop: 12 },
  primaryButton: { flex: 1, minHeight: 48, borderRadius: 14, backgroundColor: "#A7F3D0", alignItems: "center", justifyContent: "center" },
  primaryText: { color: DARK, fontSize: 14, fontWeight: "900" },
  secondaryButton: { flex: 1.3, minHeight: 48, borderRadius: 14, backgroundColor: "#1E293B", alignItems: "center", justifyContent: "center" },
  secondaryText: { color: "#CBD5E1", fontSize: 13, fontWeight: "800" },
  statusCard: { marginTop: 14, borderRadius: 20, borderWidth: 1, padding: 16 },
  goodCard: { backgroundColor: "#102C29", borderColor: "#1D6659" },
  warnCard: { backgroundColor: "#2B2113", borderColor: "#6B4D20" },
  statusRow: { flexDirection: "row-reverse", alignItems: "center", justifyContent: "space-between" },
  statusTitle: { color: "#F8FAFC", fontSize: 16, fontWeight: "900" },
  statusDot: { color: "#A7F3D0", fontSize: 22, fontWeight: "900" },
  statusMessage: { color: "#CBD5E1", fontSize: 13, lineHeight: 21, textAlign: "right", marginTop: 8 },
  urlPreview: { color: "#64748B", fontSize: 11, textAlign: "right", marginTop: 9 },
  statusActions: { flexDirection: "row-reverse", alignItems: "center", justifyContent: "space-between", marginTop: 14 },
  linkButton: { paddingVertical: 10, paddingHorizontal: 4 },
  linkText: { color: "#A7F3D0", fontSize: 13, fontWeight: "800" },
  downloadButton: { minHeight: 44, paddingHorizontal: 19, borderRadius: 13, backgroundColor: "#A7F3D0", alignItems: "center", justifyContent: "center" },
  downloadText: { color: DARK, fontSize: 13, fontWeight: "900" },
  progressWrap: { height: 28, backgroundColor: "#0B191B", borderRadius: 10, marginTop: 12, overflow: "hidden", justifyContent: "center" },
  progressBar: { position: "absolute", left: 0, top: 0, bottom: 0, backgroundColor: "#2DD4BF" },
  progressText: { color: "#E2E8F0", fontSize: 11, fontWeight: "800", textAlign: "center" },
  successText: { color: "#A7F3D0", fontSize: 12, fontWeight: "700", textAlign: "right", marginTop: 10 },
  errorText: { color: "#FDA4AF", fontSize: 12, lineHeight: 18, textAlign: "right", marginTop: 10 },
  note: { flexDirection: "row-reverse", alignItems: "flex-start", gap: 8, marginTop: 18, paddingBottom: 28 },
  noteIcon: { width: 20, height: 20, borderRadius: 10, backgroundColor: "#1E293B", color: "#94A3B8", textAlign: "center", fontSize: 13, lineHeight: 20, fontWeight: "900" },
  noteText: { flex: 1, color: "#64748B", fontSize: 12, lineHeight: 19, textAlign: "right" },
});
