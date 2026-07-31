/** Rounded card-tone skeleton lines — loading state for a section. */
export function SkeletonLines({ rows = 3 }: { rows?: number }) {
  return (
    <div className="flex flex-col gap-2.5" aria-hidden>
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className="skeleton-bar h-3" style={{ width: `${88 - i * 14}%` }} />
      ))}
    </div>
  );
}

/** A block placeholder — for the equity chart / calibration bars while loading. */
export function SkeletonBlock({ height = 220 }: { height?: number }) {
  return <div className="skeleton-bar w-full" style={{ height }} aria-hidden />;
}

/** A table-shaped skeleton — header hairline + a handful of row lines. */
export function SkeletonTable({ rows = 4 }: { rows?: number }) {
  return (
    <div aria-hidden>
      <div className="skeleton-bar mb-3 h-2.5 w-40" />
      <div className="flex flex-col gap-3 border-t border-line pt-3">
        {Array.from({ length: rows }).map((_, i) => (
          <div key={i} className="skeleton-bar h-3" style={{ width: `${92 - (i % 3) * 10}%` }} />
        ))}
      </div>
    </div>
  );
}
