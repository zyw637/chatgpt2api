"use client";

import { useEffect, useMemo, useRef, useState, type CSSProperties, type ImgHTMLAttributes } from "react";

import { ApiLoadingMark } from "@/components/api-loading-mark";
import {
  fetchCachedAuthenticatedImage,
  invalidateAuthenticatedImageCacheForSrc,
  releaseCachedAuthenticatedImage,
  resolveImageRequestURL,
  retainCachedAuthenticatedImage,
  shouldUseAuthenticatedImageFallback,
} from "@/lib/authenticated-image";
import { cn } from "@/lib/utils";

type AuthenticatedImageProps = Omit<ImgHTMLAttributes<HTMLImageElement>, "src"> & {
  src: string;
  placeholderClassName?: string;
};

function positiveNumericDimension(value: string | number | undefined) {
  const numeric = Number(value);
  return Number.isFinite(numeric) && numeric > 0 ? numeric : 0;
}

export function AuthenticatedImage({ alt, className, placeholderClassName, src, style, ...props }: AuthenticatedImageProps) {
  const [objectSrc, setObjectSrc] = useState("");
  const [fallbackToDirectSrc, setFallbackToDirectSrc] = useState(false);
  const [retainedCacheKey, setRetainedCacheKey] = useState("");
  const [loadFailed, setLoadFailed] = useState(false);
  const [reloadToken, setReloadToken] = useState(0);
  // Survives reloadToken-driven effect re-runs so a revoked/bad blob only retries once.
  const blobRetryUsedRef = useRef(false);
  const previousSrcRef = useRef(src);
  const directSrc = useMemo(() => {
    if (!src) {
      return "";
    }
    try {
      return resolveImageRequestURL(src);
    } catch {
      return src;
    }
  }, [src]);
  const shouldFetchWithAuth = useMemo(() => shouldUseAuthenticatedImageFallback(src), [src]);

  useEffect(() => {
    // Only reset retry budget when the logical image source changes — not on blob reload.
    if (previousSrcRef.current !== src) {
      previousSrcRef.current = src;
      blobRetryUsedRef.current = false;
    }

    setLoadFailed(false);
    setFallbackToDirectSrc(false);
    if (!shouldFetchWithAuth) {
      setObjectSrc("");
      setRetainedCacheKey("");
      return;
    }

    let active = true;
    let activeCacheKey = "";
    let activeObjectURL = "";

    const releaseActive = () => {
      if (activeCacheKey) {
        releaseCachedAuthenticatedImage(activeCacheKey, activeObjectURL || undefined);
      }
      activeCacheKey = "";
      activeObjectURL = "";
    };

    const cached = retainCachedAuthenticatedImage(src);
    if (cached) {
      activeCacheKey = cached.key;
      activeObjectURL = cached.objectURL;
      setObjectSrc(cached.objectURL);
      setRetainedCacheKey(cached.key);
      return () => {
        active = false;
        releaseActive();
      };
    }
    setObjectSrc("");
    setRetainedCacheKey("");

    void fetchCachedAuthenticatedImage(src)
      .then((image) => {
        if (!active) {
          releaseCachedAuthenticatedImage(image.key, image.objectURL);
          return;
        }
        if (activeCacheKey && (activeCacheKey !== image.key || activeObjectURL !== image.objectURL)) {
          releaseCachedAuthenticatedImage(activeCacheKey, activeObjectURL || undefined);
        }
        activeCacheKey = image.key;
        activeObjectURL = image.objectURL;
        setObjectSrc(image.objectURL);
        setRetainedCacheKey(image.key);
      })
      .catch(() => {
        if (active) {
          // Last resort: cookie-authenticated same-origin load.
          setFallbackToDirectSrc(true);
        }
      });

    return () => {
      active = false;
      releaseActive();
    };
  }, [shouldFetchWithAuth, src, reloadToken]);

  const displaySrc = shouldFetchWithAuth
    ? objectSrc || (fallbackToDirectSrc && !loadFailed ? directSrc : "")
    : directSrc;
  const showLoadingPlaceholder = shouldFetchWithAuth && !displaySrc && !loadFailed && !fallbackToDirectSrc;
  const showEmptyPlaceholder = !displaySrc;
  const width = positiveNumericDimension(props.width);
  const height = positiveNumericDimension(props.height);
  const placeholderStyle: CSSProperties = {
    ...style,
    ...(width > 0 && height > 0 ? { aspectRatio: `${width} / ${height}` } : {}),
  };

  if (showEmptyPlaceholder) {
    return (
      <span
        className={cn(
          className,
          "flex min-h-24 w-full items-center justify-center bg-[#f0f0f0] text-stone-400",
          placeholderClassName,
        )}
        style={placeholderStyle}
        role={alt ? "img" : undefined}
        aria-label={typeof alt === "string" && alt ? alt : undefined}
      >
        {showLoadingPlaceholder ? <ApiLoadingMark size="media" label="正在加载图片" /> : null}
      </span>
    );
  }

  return (
    <img
      {...props}
      src={displaySrc}
      alt={alt}
      className={className}
      style={style}
      data-authenticated-image-cache-key={retainedCacheKey || undefined}
      onError={(event) => {
        // blob: URL revoked or known-bad while still mounted — invalidate cache and re-fetch once.
        // Effect cleanup on reloadToken change releases the previous retain; do not release here.
        if (shouldFetchWithAuth && objectSrc && !blobRetryUsedRef.current) {
          blobRetryUsedRef.current = true;
          invalidateAuthenticatedImageCacheForSrc(src);
          setObjectSrc("");
          setRetainedCacheKey("");
          setLoadFailed(false);
          setFallbackToDirectSrc(false);
          setReloadToken((value) => value + 1);
          return;
        }
        setLoadFailed(true);
        setObjectSrc("");
        props.onError?.(event);
      }}
    />
  );
}
