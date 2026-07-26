import webConfig from "@/constants/common-env";

const MANAGED_IMAGE_PREFIXES = ["/images/", "/image-references/", "/image-thumbnails/"] as const;
const MAX_CACHED_AUTHENTICATED_IMAGE_ENTRIES = 320;
const MAX_CACHED_AUTHENTICATED_IMAGE_BYTES = 160 * 1024 * 1024;

type CachedAuthenticatedImage = {
  objectURL: string;
  byteSize: number;
  references: number;
  lastUsedAt: number;
  /** Soft-invalidated: keep objectURL until refs hit 0, but never reuse for new retain/fetch. */
  invalid?: boolean;
};

export type RetainedAuthenticatedImage = {
  key: string;
  objectURL: string;
  byteSize: number;
};

type PendingAuthenticatedImageFetch = {
  promise: Promise<{ key: string; objectURL: string; byteSize: number }>;
  controller: AbortController;
  generation: number;
  keyGeneration: number;
};

const authenticatedImageCache = new Map<string, CachedAuthenticatedImage>();
const pendingAuthenticatedImageFetches = new Map<string, PendingAuthenticatedImageFetch>();
const authenticatedImageKeyGenerations = new Map<string, number>();
let authenticatedImageCacheBytes = 0;
let authenticatedImageCacheGeneration = 0;

function browserBaseURL() {
  if (typeof window === "undefined") {
    return "http://localhost/";
  }
  return window.location.href;
}

function apiBaseURL() {
  const value = String(webConfig.apiUrl || "").trim();
  return value ? `${value.replace(/\/$/, "")}/` : "";
}

function isManagedImagePath(pathname: string) {
  return MANAGED_IMAGE_PREFIXES.some((prefix) => pathname.startsWith(prefix));
}

function normalizeManagedCachePath(value: string) {
  return value.replace(/\\/g, "/").replace(/^\/+/, "");
}

function decodedPathSegment(value: string) {
  try {
    return decodeURIComponent(value);
  } catch {
    return value;
  }
}

function managedImageSourcePathFromURL(value: string) {
  try {
    const pathname = new URL(value).pathname;
    if (pathname.startsWith("/images/")) {
      return normalizeManagedCachePath(decodedPathSegment(pathname.slice("/images/".length)));
    }
    if (pathname.startsWith("/image-thumbnails/")) {
      const thumbnailPath = decodedPathSegment(pathname.slice("/image-thumbnails/".length));
      return normalizeManagedCachePath(thumbnailPath.replace(/\.jpg$/i, ""));
    }
    if (pathname.startsWith("/image-references/")) {
      const referencePath = normalizeManagedCachePath(decodedPathSegment(pathname.slice("/image-references/".length)));
      const markerIndex = referencePath.lastIndexOf(".refs/");
      return markerIndex > 0 ? referencePath.slice(0, markerIndex) : referencePath;
    }
  } catch {
    // Ignore invalid cache keys; they cannot match managed image paths.
  }
  return "";
}

function touchCachedAuthenticatedImage(entry: CachedAuthenticatedImage) {
  entry.lastUsedAt = Date.now();
}

function isReusableCacheEntry(entry: CachedAuthenticatedImage | undefined): entry is CachedAuthenticatedImage {
  return Boolean(entry && !entry.invalid && entry.objectURL);
}

function retainAuthenticatedImageCacheEntry(key: string, entry: CachedAuthenticatedImage): RetainedAuthenticatedImage {
  touchCachedAuthenticatedImage(entry);
  entry.references += 1;
  return { key, objectURL: entry.objectURL, byteSize: entry.byteSize };
}

function disposeCacheEntry(key: string, entry: CachedAuthenticatedImage) {
  URL.revokeObjectURL(entry.objectURL);
  authenticatedImageCacheBytes = Math.max(0, authenticatedImageCacheBytes - entry.byteSize);
  authenticatedImageCache.delete(key);
}

function trimAuthenticatedImageCache() {
  while (
    authenticatedImageCache.size > MAX_CACHED_AUTHENTICATED_IMAGE_ENTRIES ||
    authenticatedImageCacheBytes > MAX_CACHED_AUTHENTICATED_IMAGE_BYTES
  ) {
    let evictableKey = "";
    let evictableEntry: CachedAuthenticatedImage | null = null;
    for (const [key, entry] of authenticatedImageCache) {
      if (entry.references > 0) {
        continue;
      }
      if (!evictableEntry || entry.lastUsedAt < evictableEntry.lastUsedAt) {
        evictableKey = key;
        evictableEntry = entry;
      }
    }
    if (!evictableEntry) {
      return;
    }
    disposeCacheEntry(evictableKey, evictableEntry);
  }
}

/**
 * Publish a blob under `key`. Never revoke an object URL that still has live references;
 * in-use superseded entries are moved to a detached key until release drops their refs to 0.
 */
function storeAuthenticatedImageCacheEntry(key: string, objectURL: string, byteSize: number): string {
  const existing = authenticatedImageCache.get(key);
  if (existing) {
    if (existing.references > 0) {
      // Keep the in-use blob alive under a detached key; mount the new blob under the real key.
      authenticatedImageCache.delete(key);
      authenticatedImageCacheBytes = Math.max(0, authenticatedImageCacheBytes - existing.byteSize);
      const detachedKey = `${key}#detached-${existing.objectURL}`;
      existing.invalid = true;
      authenticatedImageCache.set(detachedKey, existing);
      authenticatedImageCacheBytes += existing.byteSize;
    } else {
      disposeCacheEntry(key, existing);
    }
  }
  authenticatedImageCache.set(key, {
    objectURL,
    byteSize,
    references: 0,
    lastUsedAt: Date.now(),
  });
  authenticatedImageCacheBytes += byteSize;
  trimAuthenticatedImageCache();
  return objectURL;
}

export function resolveImageRequestURL(src: string) {
  const value = String(src || "").trim();
  if (!value) {
    return "";
  }

  const browserBase = browserBaseURL();
  const apiBase = apiBaseURL();
  const candidate = new URL(value, browserBase);
  if (isManagedImagePath(candidate.pathname)) {
    // The API can expose canonical public URLs even when this UI is connected to a
    // local or otherwise different instance. Managed files belong to the API that
    // served the page, so read them from that API and keep the canonical URL only
    // as application data for sharing/copying.
    const activeAPIBase = apiBase || (typeof window !== "undefined" ? `${window.location.origin}/` : "");
    if (activeAPIBase) {
      return new URL(`${candidate.pathname}${candidate.search}`, activeAPIBase).toString();
    }
  }

  return candidate.toString();
}

export function isManagedImageURL(src: string) {
  try {
    return isManagedImagePath(new URL(resolveImageRequestURL(src)).pathname);
  } catch {
    return false;
  }
}

export function shouldUseAuthenticatedImageFallback(src: string) {
  const value = String(src || "").trim();
  return Boolean(value) && !value.startsWith("data:") && !value.startsWith("blob:") && isManagedImageURL(value);
}

export async function fetchAuthenticatedImageBlob(src: string, signal?: AbortSignal) {
  const requestURL = resolveImageRequestURL(src);
  const managedImage = isManagedImageURL(src);

  const response = await fetch(requestURL, {
    signal,
    credentials: managedImage ? "include" : "same-origin",
  });
  if (!response.ok) {
    throw new Error(`读取图片失败 (${response.status})`);
  }
  const blob = await response.blob();
  if (!blob || blob.size <= 0) {
    throw new Error("读取图片失败 (empty body)");
  }
  // Reject obvious non-image error bodies (HTML/JSON) that can slip through as 200.
  // Allow empty type and application/octet-stream — browsers often omit MIME for opaque blobs.
  const contentType = String(blob.type || "").toLowerCase();
  if (
    contentType &&
    !contentType.startsWith("image/") &&
    contentType !== "application/octet-stream" &&
    !contentType.startsWith("application/octet-stream;")
  ) {
    throw new Error(`读取图片失败 (unexpected type ${blob.type})`);
  }
  return blob;
}

export function retainCachedAuthenticatedImage(src: string): RetainedAuthenticatedImage | null {
  const key = resolveImageRequestURL(src);
  const entry = authenticatedImageCache.get(key);
  return isReusableCacheEntry(entry) ? retainAuthenticatedImageCacheEntry(key, entry) : null;
}

function authenticatedImageKeyGeneration(key: string) {
  return authenticatedImageKeyGenerations.get(key) ?? 0;
}

async function loadAuthenticatedImageIntoCache(
  src: string,
  key: string,
  generation: number,
  keyGeneration: number,
  signal: AbortSignal,
) {
  const blob = await fetchAuthenticatedImageBlob(src, signal);
  const objectURL = URL.createObjectURL(blob);
  if (
    generation !== authenticatedImageCacheGeneration ||
    keyGeneration !== authenticatedImageKeyGeneration(key)
  ) {
    URL.revokeObjectURL(objectURL);
    throw new Error("图片缓存已重置");
  }
  const storedURL = storeAuthenticatedImageCacheEntry(key, objectURL, blob.size);
  const entry = authenticatedImageCache.get(key);
  return { key, objectURL: storedURL, byteSize: entry?.byteSize ?? blob.size };
}

export async function fetchCachedAuthenticatedImage(src: string, depth = 0): Promise<RetainedAuthenticatedImage> {
  if (depth > 2) {
    throw new Error("图片缓存不可用");
  }

  const key = resolveImageRequestURL(src);
  const cached = authenticatedImageCache.get(key);
  if (isReusableCacheEntry(cached)) {
    return retainAuthenticatedImageCacheEntry(key, cached);
  }

  const generation = authenticatedImageCacheGeneration;
  const keyGeneration = authenticatedImageKeyGeneration(key);
  let pending = pendingAuthenticatedImageFetches.get(key);
  if (!pending) {
    const controller = new AbortController();
    let request!: PendingAuthenticatedImageFetch;
    const promise = loadAuthenticatedImageIntoCache(src, key, generation, keyGeneration, controller.signal).finally(
      () => {
        if (pendingAuthenticatedImageFetches.get(key) === request) {
          pendingAuthenticatedImageFetches.delete(key);
        }
      },
    );
    request = { promise, controller, generation, keyGeneration };
    pending = request;
    pendingAuthenticatedImageFetches.set(key, pending);
  }

  try {
    await pending.promise;
  } catch (error) {
    const retryCached = authenticatedImageCache.get(key);
    if (isReusableCacheEntry(retryCached)) {
      return retainAuthenticatedImageCacheEntry(key, retryCached);
    }
    if (
      pending.generation !== authenticatedImageCacheGeneration ||
      pending.keyGeneration !== authenticatedImageKeyGeneration(key)
    ) {
      return fetchCachedAuthenticatedImage(src, depth + 1);
    }
    throw error;
  }

  const entry = authenticatedImageCache.get(key);
  if (!isReusableCacheEntry(entry)) {
    return fetchCachedAuthenticatedImage(src, depth + 1);
  }
  return retainAuthenticatedImageCacheEntry(key, entry);
}

/**
 * Release a retained image. Prefer matching by objectURL so detach/replace cannot
 * decrement the wrong generation of the same cache key.
 */
export function releaseCachedAuthenticatedImage(key: string, objectURL?: string) {
  if (objectURL) {
    for (const [candidateKey, candidate] of authenticatedImageCache) {
      if (candidate.objectURL !== objectURL) {
        continue;
      }
      candidate.references = Math.max(0, candidate.references - 1);
      touchCachedAuthenticatedImage(candidate);
      if (candidate.references === 0 && (candidate.invalid || candidateKey.includes("#detached-"))) {
        disposeCacheEntry(candidateKey, candidate);
        return;
      }
      if (candidate.references === 0) {
        trimAuthenticatedImageCache();
      }
      return;
    }
    // objectURL was already disposed (or never stored) — do not fall through to key-based
    // release, which can decrement a different generation of the same cache key.
    return;
  }

  const entry = authenticatedImageCache.get(key);
  if (entry) {
    entry.references = Math.max(0, entry.references - 1);
    touchCachedAuthenticatedImage(entry);
    if (entry.invalid && entry.references === 0) {
      disposeCacheEntry(key, entry);
      return;
    }
    trimAuthenticatedImageCache();
    return;
  }

  // Detached in-use entries are stored under key#detached-...
  for (const [candidateKey, candidate] of authenticatedImageCache) {
    if (!candidateKey.startsWith(`${key}#detached-`)) {
      continue;
    }
    candidate.references = Math.max(0, candidate.references - 1);
    touchCachedAuthenticatedImage(candidate);
    if (candidate.references === 0) {
      disposeCacheEntry(candidateKey, candidate);
    }
    return;
  }
}

export function getCachedAuthenticatedImageByteSize(src: string) {
  try {
    const entry = authenticatedImageCache.get(resolveImageRequestURL(src));
    if (!entry || entry.invalid) {
      return 0;
    }
    touchCachedAuthenticatedImage(entry);
    return entry.byteSize;
  } catch {
    return 0;
  }
}

function softInvalidateEntry(key: string, entry: CachedAuthenticatedImage) {
  if (entry.references > 0) {
    entry.invalid = true;
    touchCachedAuthenticatedImage(entry);
    return;
  }
  disposeCacheEntry(key, entry);
}

function invalidatePendingAuthenticatedImageFetch(key: string) {
  authenticatedImageKeyGenerations.set(key, authenticatedImageKeyGeneration(key) + 1);
  const pending = pendingAuthenticatedImageFetches.get(key);
  if (pending) {
    pending.controller.abort();
    pendingAuthenticatedImageFetches.delete(key);
  }
}

export function invalidateAuthenticatedImageCacheForPaths(paths: string[]) {
  const pathSet = new Set(paths.map(normalizeManagedCachePath));
  for (const key of pendingAuthenticatedImageFetches.keys()) {
    const sourcePath = managedImageSourcePathFromURL(key);
    if (sourcePath && pathSet.has(sourcePath)) {
      invalidatePendingAuthenticatedImageFetch(key);
    }
  }
  for (const [key, entry] of authenticatedImageCache) {
    const sourcePath = managedImageSourcePathFromURL(key.split("#detached-")[0] || key);
    if (sourcePath && pathSet.has(sourcePath)) {
      softInvalidateEntry(key, entry);
    }
  }
}

/**
 * Soft-invalidate the cache entry for one image URL so the next fetch cannot reuse a
 * known-bad blob. Live retainers keep their objectURL until release.
 */
export function invalidateAuthenticatedImageCacheForSrc(src: string) {
  const value = String(src || "").trim();
  if (!value) {
    return;
  }
  let key = "";
  try {
    key = resolveImageRequestURL(value);
  } catch {
    return;
  }
  if (!key) {
    return;
  }
  invalidatePendingAuthenticatedImageFetch(key);
  const entry = authenticatedImageCache.get(key);
  if (entry) {
    softInvalidateEntry(key, entry);
  }
  for (const [candidateKey, candidate] of authenticatedImageCache) {
    if (candidateKey.startsWith(`${key}#detached-`)) {
      softInvalidateEntry(candidateKey, candidate);
    }
  }
}

export function clearAuthenticatedImageCache() {
  authenticatedImageCacheGeneration += 1;
  for (const pending of pendingAuthenticatedImageFetches.values()) {
    pending.controller.abort();
  }
  pendingAuthenticatedImageFetches.clear();
  authenticatedImageKeyGenerations.clear();
  for (const [key, entry] of authenticatedImageCache) {
    softInvalidateEntry(key, entry);
  }
  // Recompute bytes from survivors (still-referenced soft-invalidated entries).
  authenticatedImageCacheBytes = 0;
  for (const entry of authenticatedImageCache.values()) {
    authenticatedImageCacheBytes += entry.byteSize;
  }
}

/** Test-only snapshot of cache bookkeeping. Not for production UI. */
export function getAuthenticatedImageCacheDebugState() {
  const entries = Array.from(authenticatedImageCache.entries()).map(([key, entry]) => ({
    key,
    byteSize: entry.byteSize,
    references: entry.references,
    invalid: Boolean(entry.invalid),
    objectURL: entry.objectURL,
  }));
  return {
    size: authenticatedImageCache.size,
    bytes: authenticatedImageCacheBytes,
    generation: authenticatedImageCacheGeneration,
    pending: pendingAuthenticatedImageFetches.size,
    entries,
  };
}
