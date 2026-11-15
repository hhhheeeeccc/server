import { useCallback, useState } from "react";
import { Alert, FlatList, Linking, Platform, Share, StyleSheet, Text, TouchableOpacity, View } from "react-native";
import { useFocusEffect } from "expo-router";
import * as Sharing from "expo-sharing";

import { ScreenContainer } from "@/components/screen-container";
import { loadSavedVideos, removeSavedVideo, type SavedVideo } from "@/lib/download-history";

function formatDate(value: string) {
  try {
    return new Intl.DateTimeFormat("ar", { day: "numeric", month: "short", year: "numeric" }).format(new Date(value));
  } catch {
    return value;
  }
}

export default function SavedScreen() {
  const [items, setItems] = useState<SavedVideo[]>([]);

  useFocusEffect(
    useCallback(() => {
      loadSavedVideos().then(setItems);
    }, []),
  );

  const share = async (item: SavedVideo) => {
    if (Platform.OS === "web") {
      await Share.share({ message: item.uri });
      return;
    }
    const available = await Sharing.isAvailableAsync();
    if (available) await Sharing.shareAsync(item.uri, { mimeType: "video/mp4", dialogTitle: "مشاركة الفيديو" });
  };

  const open = async (item: SavedVideo) => {
    try {
      await Linking.openURL(item.uri);
    } catch {
      Alert.alert("تعذر فتح الملف", "ثبت تطبيقًا مناسبًا لتشغيل الفيديو ثم حاول مرة أخرى.");
    }
  };

  const remove = (item: SavedVideo) => {
    Alert.alert("حذف من السجل؟", item.filename, [
      { text: "إلغاء", style: "cancel" },
      { text: "حذف", style: "destructive", onPress: async () => setItems(await removeSavedVideo(item.id)) },
    ]);
  };

  return (
    <ScreenContainer safeAreaClassName="bg-slate-950" className="bg-slate-950 px-5 pt-4">
      <View style={styles.header}>
        <Text style={styles.eyebrow}>مكتبتك المحلية</Text>
        <Text style={styles.title}>محفوظاتي</Text>
        <Text style={styles.subtitle}>الملفات التي حفظتها داخل VideoLoad على هذا الجهاز.</Text>
      </View>
      <FlatList
        data={items}
        keyExtractor={(item) => item.id}
        contentContainerStyle={items.length ? styles.list : styles.emptyList}
        showsVerticalScrollIndicator={false}
        renderItem={({ item }) => (
          <View style={styles.card}>
            <View style={styles.fileIcon}><Text style={styles.fileIconText}>▶</Text></View>
            <View style={styles.fileInfo}>
              <Text style={styles.filename} numberOfLines={1}>{item.filename}</Text>
              <Text style={styles.meta}>{item.source}  •  {formatDate(item.createdAt)}</Text>
              <View style={styles.actions}>
                <TouchableOpacity onPress={() => open(item)} style={styles.actionButton}><Text style={styles.actionText}>فتح</Text></TouchableOpacity>
                <TouchableOpacity onPress={() => share(item)} style={styles.actionButton}><Text style={styles.actionText}>مشاركة</Text></TouchableOpacity>
                <TouchableOpacity onPress={() => remove(item)} style={styles.deleteButton}><Text style={styles.deleteText}>حذف</Text></TouchableOpacity>
              </View>
            </View>
          </View>
        )}
        ListEmptyComponent={<View style={styles.empty}><Text style={styles.emptyIcon}>▱</Text><Text style={styles.emptyTitle}>لا توجد ملفات بعد</Text><Text style={styles.emptyText}>ارجع إلى الرئيسية وشارك رابط فيديو مباشر مسموح بحفظه.</Text></View>}
      />
    </ScreenContainer>
  );
}

const styles = StyleSheet.create({
  header: { paddingTop: 12, paddingBottom: 22 },
  eyebrow: { color: "#A7F3D0", fontSize: 13, fontWeight: "700", textAlign: "right", marginBottom: 8 },
  title: { color: "#F8FAFC", fontSize: 32, fontWeight: "800", textAlign: "right" },
  subtitle: { color: "#94A3B8", fontSize: 14, lineHeight: 22, textAlign: "right", marginTop: 8 },
  list: { paddingBottom: 32, gap: 12 },
  emptyList: { flexGrow: 1, justifyContent: "center", paddingBottom: 60 },
  card: { flexDirection: "row-reverse", alignItems: "center", backgroundColor: "#111827", borderColor: "#1E293B", borderWidth: 1, borderRadius: 20, padding: 14, gap: 12 },
  fileIcon: { width: 48, height: 48, borderRadius: 15, backgroundColor: "#163B35", justifyContent: "center", alignItems: "center" },
  fileIconText: { color: "#A7F3D0", fontSize: 18 },
  fileInfo: { flex: 1, alignItems: "flex-end" },
  filename: { color: "#F8FAFC", fontSize: 15, fontWeight: "700", maxWidth: "100%" },
  meta: { color: "#64748B", fontSize: 12, marginTop: 4 },
  actions: { flexDirection: "row-reverse", gap: 8, marginTop: 10 },
  actionButton: { backgroundColor: "#1E293B", borderRadius: 9, paddingHorizontal: 12, paddingVertical: 7 },
  actionText: { color: "#CBD5E1", fontSize: 12, fontWeight: "700" },
  deleteButton: { paddingHorizontal: 8, paddingVertical: 7 },
  deleteText: { color: "#FB7185", fontSize: 12, fontWeight: "700" },
  empty: { alignItems: "center", paddingHorizontal: 20 },
  emptyIcon: { color: "#334155", fontSize: 56, marginBottom: 10 },
  emptyTitle: { color: "#F8FAFC", fontSize: 20, fontWeight: "800" },
  emptyText: { color: "#94A3B8", fontSize: 14, lineHeight: 22, textAlign: "center", marginTop: 8 },
});
