import { useCallback } from "react";

export function clearSharedPayloads() {}

export function useSafeIncomingShare() {
  return {
    resolvedSharedPayloads: [],
    sharedPayloads: [],
    isResolving: false,
    error: null,
    refreshSharePayloads: useCallback(() => {}, []),
    clearSharedPayloads,
  };
}
