import { describe, expect, it } from "vitest";
import {
  detectSource,
  inspectVideoLink,
  normalizeSharedText,
  safeVideoFilename,
} from "../lib/video-link";

describe("video link inspection", () => {
  it.each([
    ["https://www.snapchat.com/spotlight/abc", "Snapchat"],
    ["https://instagram.com/reel/abc", "Instagram"],
    ["https://t.me/channel/123", "Telegram"],
    ["https://youtube.com/watch?v=abc", "YouTube"],
    ["https://youtu.be/abc", "YouTube"],
  ] as const)("detects supported platform URL %s", (url, source) => {
    expect(detectSource(url)).toBe(source);
    const result = inspectVideoLink(url);
    expect(result.isValid).toBe(true);
    expect(result.canDownload).toBe(false);
    expect(result.isDirectMedia).toBe(false);
  });

  it.each([
    "https://not-snapchat.com/video.mp4",
    "https://snapchat.com.attacker.invalid/video.mp4",
    "https://instagram.com.attacker.invalid/reel/abc",
    "https://youtube.com.attacker.invalid/watch?v=abc",
  ])("does not misclassify deceptive hostname %s", (url) => {
    expect(detectSource(url)).not.toMatch(/Snapchat|Instagram|YouTube/);
  });

  it("allows secure direct media files, including links with query parameters", () => {
    const result = inspectVideoLink("https://cdn.example.test/path/clip.webm?signature=abc");
    expect(result.source).toBe("رابط مباشر");
    expect(result.isDirectMedia).toBe(true);
    expect(result.canDownload).toBe(true);
  });

  it("requires HTTPS for direct media downloads", () => {
    const result = inspectVideoLink("http://cdn.example.test/clip.mp4");
    expect(result.isValid).toBe(true);
    expect(result.isDirectMedia).toBe(true);
    expect(result.canDownload).toBe(false);
    expect(result.message).toContain("HTTPS");
  });

  it.each([
    "javascript:alert(1)",
    "https://user:password@example.test/clip.mp4",
    "not a URL",
  ])("rejects unsupported or credential-bearing URL %s", (url) => {
    const result = inspectVideoLink(url);
    expect(result.isValid).toBe(false);
    expect(result.canDownload).toBe(false);
    expect(result.host).toBe("");
  });

  it("recognizes media by its path, not an extension-like query parameter", () => {
    expect(inspectVideoLink("https://example.test/watch?file=clip.mp4").canDownload).toBe(false);
  });

  it("extracts a URL from shared text and removes common trailing punctuation", () => {
    expect(normalizeSharedText("شاهد هذا: https://youtu.be/abc). ")).toBe("https://youtu.be/abc");
  });

  it("sanitizes a safe filename and preserves its actual media extension", () => {
    expect(safeVideoFilename("https://example.test/%D9%85%D9%82%D8%B7%D8%B9.webm?token=1")).toBe("مقطع.webm");
    expect(safeVideoFilename("https://example.test/a%2Fb.mp4")).toBe("a-b.mp4");
    expect(safeVideoFilename("https://example.test/no-extension")).toBe("videoload-video.mp4");
  });
});
