import * as FileSystem from "expo-file-system/legacy";
import { Platform } from "react-native";
import { safeVideoFilename } from "@/lib/video-link";

export {
  detectSource,
  inspectVideoLink,
  normalizeSharedText,
  safeVideoFilename,
} from "@/lib/video-link";
export type { LinkInspection, VideoSource } from "@/lib/video-link";

export async function downloadVideo(
  url: string,
  onProgress?: (progress: number) => void,
) {
  if (!FileSystem.documentDirectory) {
    throw new Error("مساحة التخزين المحلية غير متاحة على هذا الجهاز");
  }
  const filename = `${Date.now()}-${safeVideoFilename(url)}`;
  const target = `${FileSystem.documentDirectory}${filename}`;
  const task = FileSystem.createDownloadResumable(
    url,
    target,
    {},
    (progress) => {
      const total = progress.totalBytesExpectedToWrite;
      const current = progress.totalBytesWritten;
      onProgress?.(total > 0 ? Math.round((current / total) * 100) : 0);
    },
  );
  const result = await task.downloadAsync();
  if (!result?.uri) throw new Error("تعذر الوصول إلى الملف الذي تم تنزيله");
  onProgress?.(100);
  return { uri: result.uri, filename };
}

export async function saveToDeviceLibrary(uri: string) {
  if (Platform.OS === "web") return false;
  const MediaLibrary = await import("expo-media-library");
  const permission = await MediaLibrary.requestPermissionsAsync(true);
  if (!permission.granted) return false;
  await MediaLibrary.createAssetAsync(uri);
  return true;
}
