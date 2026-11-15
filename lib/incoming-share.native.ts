import { useIncomingShare, clearSharedPayloads } from "expo-sharing";

export { clearSharedPayloads };

export function useSafeIncomingShare() {
  return useIncomingShare();
}
