import * as FileSystem from "expo-file-system/legacy";
import { Platform } from "react-native";

export type VideoSource = "Snapchat" | "Instagram" | "Telegram" | "YouTube" | "رابط مباشر" | "غير معروف";

export type LinkInspection = {
  url: string;
  source: VideoSource;
  host: string;
  isValid: boolean;
  isDirectMedia: boolean;
  canDownload: boolean;
  message: string;
};

const MEDIA_EXTENSIONS = /\.(mp4|m4v|mov|webm|mkv|avi)(?:$|\?)/i;

function getHost(url: string) {
  try {
    return new URL(url).hostname.replace(/^www\./, "").toLowerCase();
  } catch {
    return "";
  }
}

export function normalizeSharedText(value: string) {
  const trimmed = value.trim();
  const candidate = trimmed.match(/https?:\/\/[^\s<>]+/i)?.[0] ?? trimmed;
  return candidate.replace(/[),.;!?]+$/, "");
}

export function detectSource(url: string): VideoSource {
  const host = getHost(url);
  if (host.includes("snapchat.com")) return "Snapchat";
  if (host.includes("instagram.com")) return "Instagram";
  if (host.includes("telegram.org") || host.includes("t.me")) return "Telegram";
  if (host.includes("youtube.com") || host.includes("youtu.be")) return "YouTube";
  if (MEDIA_EXTENSIONS.test(url)) return "رابط مباشر";
  return "غير معروف";
}

export function inspectVideoLink(rawValue: string): LinkInspection {
  const url = normalizeSharedText(rawValue);
  let parsed: URL | null = null;
  try {
    parsed = new URL(url);
  } catch {
    return {
      url,
      source: "غير معروف",
      host: "",
      isValid: false,
      isDirectMedia: false,
      canDownload: false,
      message: "أدخل رابطًا يبدأ بـ https://",
    };
  }

  const source = detectSource(url);
  const isDirectMedia = MEDIA_EXTENSIONS.test(parsed.pathname + parsed.search);
  const isSupportedPage = ["Snapchat", "Instagram", "Telegram", "YouTube"].includes(source);
  const canDownload = isDirectMedia && parsed.protocol === "https:";

  if (isSupportedPage && !isDirectMedia) {
    return {
      url,
      source,
      host: parsed.hostname,
      isValid: true,
      isDirectMedia: false,
      canDownload: false,
      message: "هذا رابط صفحة. لا يتم تجاوز حماية المنصة؛ استخدم رابط تنزيل مباشر تملك حق استخدامه.",
    };
  }

  if (!canDownload) {
    return {
      url,
      source,
      host: parsed.hostname,
      isValid: true,
      isDirectMedia: false,
      canDownload: false,
      message: "الرابط صالح، لكنه ليس ملف فيديو مباشرًا قابلًا للحفظ من داخل التطبيق.",
    };
  }

  return {
    url,
    source,
    host: parsed.hostname,
    isValid: true,
    isDirectMedia: true,
    canDownload: true,
    message: "رابط فيديو مباشر. راجع إذن الاستخدام قبل الحفظ.",
  };
}

function safeFilename(url: string) {
  const pathname = new URL(url).pathname.split("/").pop() || "videoload-video.mp4";
  const cleaned = pathname.replace(/[^a-zA-Z0-9._-]/g, "-");
  return cleaned.toLowerCase().endsWith(".mp4") ? cleaned : `${cleaned}.mp4`;
}

export async function downloadVideo(
  url: string,
  onProgress?: (progress: number) => void,
) {
  if (!FileSystem.documentDirectory) throw new Error("مساحة التخزين المحلية غير متاحة");
  const filename = `${Date.now()}-${safeFilename(url)}`;
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
