export type VideoSource =
  | "Snapchat"
  | "Instagram"
  | "Telegram"
  | "YouTube"
  | "رابط مباشر"
  | "غير معروف";

export type LinkInspection = {
  url: string;
  source: VideoSource;
  host: string;
  isValid: boolean;
  isDirectMedia: boolean;
  canDownload: boolean;
  message: string;
};

const MEDIA_EXTENSIONS = new Set(["mp4", "m4v", "mov", "webm", "mkv", "avi"]);
const PLATFORM_HOSTS: ReadonlyArray<[VideoSource, string[]]> = [
  ["Snapchat", ["snapchat.com"]],
  ["Instagram", ["instagram.com"]],
  ["Telegram", ["telegram.org", "telegram.me", "t.me"]],
  ["YouTube", ["youtube.com", "youtu.be", "youtube-nocookie.com"]],
];

function parseHttpUrl(value: string): URL | null {
  try {
    const parsed = new URL(value);
    if (
      (parsed.protocol !== "https:" && parsed.protocol !== "http:") ||
      !parsed.hostname ||
      parsed.username.length > 0 ||
      parsed.password.length > 0
    ) {
      return null;
    }
    return parsed;
  } catch {
    return null;
  }
}

function isHostOrSubdomain(host: string, domain: string) {
  return host === domain || host.endsWith(`.${domain}`);
}

function getMediaExtension(pathname: string) {
  const match = pathname.match(/\.([a-z0-9]+)$/i);
  const extension = match?.[1]?.toLowerCase();
  return extension && MEDIA_EXTENSIONS.has(extension) ? extension : null;
}

export function normalizeSharedText(value: string) {
  const trimmed = value.trim();
  const candidate = trimmed.match(/https?:\/\/[^\s<>]+/i)?.[0] ?? trimmed;
  return candidate.replace(/[),.;!?]+$/, "");
}

export function detectSource(value: string): VideoSource {
  const parsed = parseHttpUrl(value);
  if (!parsed) return "غير معروف";
  const host = parsed.hostname.toLowerCase().replace(/^www\./, "");
  for (const [source, domains] of PLATFORM_HOSTS) {
    if (domains.some((domain) => isHostOrSubdomain(host, domain))) return source;
  }
  return getMediaExtension(parsed.pathname) ? "رابط مباشر" : "غير معروف";
}

export function inspectVideoLink(rawValue: string): LinkInspection {
  const url = normalizeSharedText(rawValue);
  const parsed = parseHttpUrl(url);
  if (!parsed) {
    return {
      url,
      source: "غير معروف",
      host: "",
      isValid: false,
      isDirectMedia: false,
      canDownload: false,
      message: "أدخل رابطًا صحيحًا يبدأ بـ https:// أو http://، من دون بيانات تسجيل دخول داخل الرابط.",
    };
  }

  const host = parsed.hostname.toLowerCase().replace(/^www\./, "");
  const source = detectSource(url);
  const isDirectMedia = getMediaExtension(parsed.pathname) !== null;
  const canDownload = isDirectMedia && parsed.protocol === "https:";

  if (!isDirectMedia && source !== "غير معروف") {
    return {
      url,
      source,
      host,
      isValid: true,
      isDirectMedia: false,
      canDownload: false,
      message: "هذا رابط صفحة من منصة. لا يتجاوز التطبيق حماية المنصة؛ استخدم رابط ملف مباشرًا مصرحًا بتنزيله.",
    };
  }

  if (isDirectMedia && parsed.protocol !== "https:") {
    return {
      url,
      source,
      host,
      isValid: true,
      isDirectMedia: true,
      canDownload: false,
      message: "رابط الملف يستخدم اتصالًا غير مشفر. يلزم رابط HTTPS آمن قبل الحفظ.",
    };
  }

  if (!canDownload) {
    return {
      url,
      source,
      host,
      isValid: true,
      isDirectMedia: false,
      canDownload: false,
      message: "الرابط صالح، لكنه لا يشير إلى ملف فيديو مباشر يمكن حفظه من داخل التطبيق.",
    };
  }

  return {
    url,
    source,
    host,
    isValid: true,
    isDirectMedia: true,
    canDownload: true,
    message: "رابط فيديو مباشر عبر HTTPS. تأكد من امتلاكك حق استخدام المحتوى والسماح بتنزيله.",
  };
}

export function safeVideoFilename(value: string) {
  const parsed = parseHttpUrl(value);
  const rawName = parsed?.pathname.split("/").filter(Boolean).pop() ?? "";
  let decodedName = rawName;
  try {
    decodedName = decodeURIComponent(rawName);
  } catch {
    // Keep the raw path segment when it contains malformed percent-encoding.
  }
  const cleanedName = decodedName
    .replace(/[\\/:*?"<>|\u0000-\u001f]/g, "-")
    .replace(/[^\p{L}\p{N}._-]/gu, "-")
    .replace(/^[.-]+|[.-]+$/g, "")
    .slice(0, 120);
  if (cleanedName && getMediaExtension(cleanedName)) return cleanedName;
  return "videoload-video.mp4";
}
