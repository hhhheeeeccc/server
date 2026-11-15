import AsyncStorage from "@react-native-async-storage/async-storage";

const STORAGE_KEY = "videoload.download-history.v1";

export type SavedVideo = {
  id: string;
  filename: string;
  uri: string;
  source: string;
  originalUrl: string;
  createdAt: string;
  sizeLabel?: string;
};

export async function loadSavedVideos(): Promise<SavedVideo[]> {
  try {
    const value = await AsyncStorage.getItem(STORAGE_KEY);
    if (!value) return [];
    const parsed = JSON.parse(value);
    return Array.isArray(parsed) ? parsed : [];
  } catch {
    return [];
  }
}

export async function addSavedVideo(video: SavedVideo) {
  const current = await loadSavedVideos();
  const next = [video, ...current.filter((item) => item.id !== video.id)].slice(0, 50);
  await AsyncStorage.setItem(STORAGE_KEY, JSON.stringify(next));
  return next;
}

export async function removeSavedVideo(id: string) {
  const current = await loadSavedVideos();
  const next = current.filter((item) => item.id !== id);
  await AsyncStorage.setItem(STORAGE_KEY, JSON.stringify(next));
  return next;
}
