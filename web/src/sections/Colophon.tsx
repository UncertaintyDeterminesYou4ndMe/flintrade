import { SkeletonLines } from '../components/Skeleton';
import { useConfig } from '../lib/hooks';
import { toColophon } from '../lib/transform';

export function Colophon() {
  const config = useConfig();
  const view = toColophon(config.data);

  return (
    <div className="mx-auto max-w-6xl px-6 pb-20 pt-16">
      <div className="border-t border-line" />

      <div className="mt-10">
        {config.isLoading ? (
          <SkeletonLines rows={6} />
        ) : (
          <div className="grid grid-cols-1 gap-10 text-[13px] leading-loose text-muted md:grid-cols-3">
            <div>
              <h3 className="mb-3 text-[13px] font-semibold text-fg">House Rules</h3>
              {view.houseRules.length ? (
                view.houseRules.map((r) => (
                  <div key={r.label} className="flex justify-between gap-4 border-b border-line py-1">
                    <span>{r.label}</span>
                    <span className="tabular-nums text-fg">{r.value}</span>
                  </div>
                ))
              ) : (
                <p className="text-faint">unavailable</p>
              )}
            </div>

            <div>
              <h3 className="mb-3 text-[13px] font-semibold text-fg">The Universe</h3>
              <p className="flex flex-wrap gap-1.5">
                {view.universe.length ? (
                  view.universe.map((sym) => (
                    <span key={sym} className="rounded-md bg-card px-2 py-0.5 text-[12px] text-fg">
                      {sym}
                    </span>
                  ))
                ) : (
                  <span className="text-faint">unavailable</span>
                )}
              </p>
            </div>

            <div>
              <h3 className="mb-3 text-[13px] font-semibold text-fg">Cadence</h3>
              {view.cadence.length ? (
                view.cadence.map((c) => (
                  <div key={c.label} className="flex justify-between gap-4 border-b border-line py-1">
                    <span>{c.label}</span>
                    <span className="tabular-nums text-fg">{c.value}</span>
                  </div>
                ))
              ) : (
                <p className="text-faint">unavailable</p>
              )}
            </div>
          </div>
        )}
      </div>

      <p className="mt-12 text-[13px] text-faint">
        Set by machine, read by no one in particular. Paper trading only — not financial advice.
      </p>
    </div>
  );
}
