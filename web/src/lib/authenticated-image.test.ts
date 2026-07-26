import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@/constants/common-env", () => ({
  default: {
    apiUrl: "",
    appVersion: "test",
  },
}));

import {
  clearAuthenticatedImageCache,
  fetchCachedAuthenticatedImage,
  getAuthenticatedImageCacheDebugState,
  invalidateAuthenticatedImageCacheForSrc,
  releaseCachedAuthenticatedImage,
  resolveImageRequestURL,
  retainCachedAuthenticatedImage,
} from "./authenticated-image";

function pngBlob(size = 64) {
  return new Blob([new Uint8Array(size).fill(1)], { type: "image/png" });
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

describe("authenticated image cache", () => {
  let objectURLSeq = 0;
  const revoked = new Set<string>();
  const realCreateObjectURL = URL.createObjectURL.bind(URL);
  const realRevokeObjectURL = URL.revokeObjectURL.bind(URL);

  beforeEach(() => {
    revoked.clear();
    clearAuthenticatedImageCache();

    vi.stubGlobal("window", {
      location: {
        href: "http://localhost/app",
        origin: "http://localhost",
      },
    });

    URL.createObjectURL = vi.fn(() => {
      objectURLSeq += 1;
      return `blob:http://localhost/mock-${objectURLSeq}`;
    }) as typeof URL.createObjectURL;
    URL.revokeObjectURL = vi.fn((url: string) => {
      revoked.add(url);
    }) as typeof URL.revokeObjectURL;
  });

  afterEach(() => {
    clearAuthenticatedImageCache();
    URL.createObjectURL = realCreateObjectURL;
    URL.revokeObjectURL = realRevokeObjectURL;
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("reads canonical managed image URLs from the active app instance", () => {
    expect(resolveImageRequestURL("https://images.example.test/images/2026/07/new.png")).toBe(
      "http://localhost/images/2026/07/new.png",
    );
    expect(
      resolveImageRequestURL("https://images.example.test/image-thumbnails/2026/07/new.png.jpg?v=3-123"),
    ).toBe("http://localhost/image-thumbnails/2026/07/new.png.jpg?v=3-123");
    expect(resolveImageRequestURL("https://cdn.example.test/unmanaged/image.png")).toBe(
      "https://cdn.example.test/unmanaged/image.png",
    );
  });

  it("uses cookie credentials without exposing a bearer token", async () => {
    const fetchMock = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) =>
      new Response(pngBlob(), { status: 200, headers: { "Content-Type": "image/png" } }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const retained = await fetchCachedAuthenticatedImage("/images/2026/07/21/cookie-only.png");

    const [, init] = fetchMock.mock.calls[0];
    expect(init?.credentials).toBe("include");
    expect(init?.headers).toBeUndefined();
    releaseCachedAuthenticatedImage(retained.key, retained.objectURL);
  });

  it("keeps objectURL alive while retained across clearAuthenticatedImageCache", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(pngBlob(128), { status: 200, headers: { "Content-Type": "image/png" } })),
    );

    const src = "/images/2026/07/21/sample.png";
    const retained = await fetchCachedAuthenticatedImage(src);
    expect(retained.objectURL).toMatch(/^blob:/);
    expect(getAuthenticatedImageCacheDebugState().entries.some((entry) => entry.references === 1)).toBe(true);

    clearAuthenticatedImageCache();
    const afterClear = getAuthenticatedImageCacheDebugState();
    const live = afterClear.entries.find((entry) => entry.objectURL === retained.objectURL);
    expect(live).toBeTruthy();
    expect(live?.invalid).toBe(true);
    expect(live?.references).toBe(1);
    expect(revoked.has(retained.objectURL)).toBe(false);

    // Soft-invalidated entry must not be reused for new retains.
    expect(retainCachedAuthenticatedImage(src)).toBeNull();

    releaseCachedAuthenticatedImage(retained.key, retained.objectURL);
    expect(revoked.has(retained.objectURL)).toBe(true);
    expect(getAuthenticatedImageCacheDebugState().entries.find((entry) => entry.objectURL === retained.objectURL)).toBeUndefined();
  });

  it("refetches after invalidateAuthenticatedImageCacheForSrc instead of reusing a bad blob", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response(pngBlob(32), { status: 200, headers: { "Content-Type": "image/png" } }))
      .mockResolvedValueOnce(new Response(pngBlob(96), { status: 200, headers: { "Content-Type": "image/png" } }));
    vi.stubGlobal("fetch", fetchMock);

    const src = "/images/2026/07/21/bad-then-good.png";
    const first = await fetchCachedAuthenticatedImage(src);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    invalidateAuthenticatedImageCacheForSrc(src);
    expect(retainCachedAuthenticatedImage(src)).toBeNull();

    const second = await fetchCachedAuthenticatedImage(src);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(second.objectURL).not.toBe(first.objectURL);

    // First blob stays until its retain is released.
    expect(revoked.has(first.objectURL)).toBe(false);
    releaseCachedAuthenticatedImage(first.key, first.objectURL);
    expect(revoked.has(first.objectURL)).toBe(true);

    releaseCachedAuthenticatedImage(second.key, second.objectURL);
  });

  it("does not let an invalidated in-flight request overwrite or remove its replacement", async () => {
    const firstResponse = deferred<Response>();
    const secondResponse = deferred<Response>();
    const fetchMock = vi
      .fn()
      .mockImplementationOnce(() => firstResponse.promise)
      .mockImplementationOnce(() => secondResponse.promise);
    vi.stubGlobal("fetch", fetchMock);

    const src = "/images/2026/07/21/in-flight.png";
    const firstLoad = fetchCachedAuthenticatedImage(src);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    const firstSignal = fetchMock.mock.calls[0]?.[1]?.signal;
    invalidateAuthenticatedImageCacheForSrc(src);
    expect(firstSignal?.aborted).toBe(true);

    const secondLoad = fetchCachedAuthenticatedImage(src);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(getAuthenticatedImageCacheDebugState().pending).toBe(1);

    // Simulate a transport that ignores abort and completes the stale request late.
    firstResponse.resolve(new Response(pngBlob(32), { status: 200, headers: { "Content-Type": "image/png" } }));
    await vi.waitFor(() => {
      expect(URL.revokeObjectURL).toHaveBeenCalledTimes(1);
    });
    expect(getAuthenticatedImageCacheDebugState().pending).toBe(1);
    expect(getAuthenticatedImageCacheDebugState().size).toBe(0);

    secondResponse.resolve(new Response(pngBlob(96), { status: 200, headers: { "Content-Type": "image/png" } }));
    const [first, second] = await Promise.all([firstLoad, secondLoad]);

    expect(first.objectURL).toBe(second.objectURL);
    expect(first.byteSize).toBe(96);
    expect(getAuthenticatedImageCacheDebugState().pending).toBe(0);
    expect(getAuthenticatedImageCacheDebugState().entries).toHaveLength(1);
    expect(getAuthenticatedImageCacheDebugState().entries[0]?.references).toBe(2);

    releaseCachedAuthenticatedImage(first.key, first.objectURL);
    releaseCachedAuthenticatedImage(second.key, second.objectURL);
  });

  it("does not decrement a newer generation when releasing a missing objectURL", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(pngBlob(40), { status: 200, headers: { "Content-Type": "image/png" } })),
    );

    const src = "/images/2026/07/21/refcount.png";
    const retained = await fetchCachedAuthenticatedImage(src);
    expect(getAuthenticatedImageCacheDebugState().entries[0]?.references).toBe(1);

    // Simulate a stale cleanup that still knows the key but not the live objectURL.
    releaseCachedAuthenticatedImage(retained.key, "blob:http://localhost/already-gone");
    expect(getAuthenticatedImageCacheDebugState().entries[0]?.references).toBe(1);

    releaseCachedAuthenticatedImage(retained.key, retained.objectURL);
    // Valid zero-ref entries may stay until trim; the important part is refs hit 0
    // without the stale objectURL release double-decrementing.
    const after = getAuthenticatedImageCacheDebugState().entries.find((entry) => entry.key === retained.key);
    expect(after?.references ?? 0).toBe(0);
    expect(revoked.has(retained.objectURL)).toBe(false);
  });
});
