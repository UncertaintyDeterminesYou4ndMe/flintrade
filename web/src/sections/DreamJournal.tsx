import type { CSSProperties } from 'react';
import { SkeletonLines, SkeletonTable } from '../components/Skeleton';
import { useBrain, useReviews } from '../lib/hooks';
import { toDreamView, type CalibrationRow, type LessonRow } from '../lib/transform';

function CalibrationBar({ row }: { row: CalibrationRow }) {
  return (
    <div className="py-2">
      <div className="flex items-baseline justify-between text-[12px] tracking-wide text-muted">
        <span>conf {row.bucket}</span>
        <span className="tabular-nums">
          {row.hasData ? `${row.winRatePct!.toFixed(1)}% · n=${row.n}` : `— · n=${row.n}`}
        </span>
      </div>
      <div className="mt-1.5 h-2 w-full rounded-full bg-card">
        {row.hasData && (
          <div
            className={`h-full rounded-full ${row.belowHalf ? 'bg-down' : 'bg-up'}`}
            style={{ width: `${row.barPct}%` }}
          />
        )}
      </div>
    </div>
  );
}

function LessonEntry({ lesson, index }: { lesson: LessonRow; index: number }) {
  return (
    <div
      className="reveal reveal-in border-l-2 py-1 pl-5"
      style={{ borderColor: lesson.strong ? '#30D158' : '#3A3A3C', '--i': index } as CSSProperties}
    >
      <p className="text-[15px] leading-snug text-fg">{lesson.text}</p>
      <p className="mt-1.5 text-[11px] tracking-wide text-muted">
        confidence {lesson.confidence.toFixed(2)}
        {lesson.n !== null ? ` · n=${lesson.n}` : ''}
      </p>
      <div className="mt-1 h-0.5 w-full max-w-[12rem] rounded-full bg-card">
        <div className="h-full rounded-full bg-fg" style={{ width: `${lesson.barPct}%` }} />
      </div>
    </div>
  );
}

export function DreamJournal() {
  const brain = useBrain();
  const reviews = useReviews();

  const loading = brain.isLoading || reviews.isLoading;
  const view = toDreamView(brain.data, reviews.data);

  return (
    <div className="mx-auto max-w-6xl px-6 py-16">
      <h2 className="text-[22px] font-semibold tracking-tight text-fg">Dream Journal</h2>

      <div className="mt-8 grid grid-cols-1 gap-12 lg:grid-cols-[1fr_2fr]">
        {/* left — self-assessment + calibration */}
        <div>
          <h3 className="text-[13px] font-medium uppercase tracking-wide text-muted">Self-Assessment</h3>
          {loading ? (
            <div className="mt-4">
              <SkeletonLines rows={5} />
            </div>
          ) : (
            <div className="mt-4 rounded-2xl bg-card p-5">
              <div className="flex flex-wrap gap-x-8 gap-y-3 text-[28px] font-semibold tabular-nums text-fg">
                <div>
                  {view.winRateLabel}
                  <div className="mt-1 text-[11px] font-normal tracking-wide text-muted">win rate</div>
                </div>
                <div>
                  {view.n ?? '—'}
                  <div className="mt-1 text-[11px] font-normal tracking-wide text-muted">n</div>
                </div>
                <div className={view.netPnlNegative ? 'text-down' : 'text-up'}>
                  {view.netPnlLabel}
                  <div className="mt-1 text-[11px] font-normal tracking-wide text-muted">net p&amp;l</div>
                </div>
              </div>
              {view.hasSelfAssessment && view.note && (
                <p className="mt-4 border-t border-line pt-4 text-[14px] leading-relaxed text-fg">{view.note}</p>
              )}
              {!view.hasSelfAssessment && (
                <p className="mt-4 border-t border-line pt-4 text-[14px] text-faint">No self-assessment yet.</p>
              )}
            </div>
          )}

          <div className="mt-8">
            {view.calibration.map((row) => (
              <CalibrationBar key={row.bucket} row={row} />
            ))}
          </div>
        </div>

        {/* right — Lessons, Plans, Setup Matrix */}
        <div>
          <h3 className="text-[13px] font-medium uppercase tracking-wide text-muted">Lessons</h3>
          {loading ? (
            <div className="mt-4">
              <SkeletonLines rows={5} />
            </div>
          ) : view.lessons.length ? (
            <div className="mt-4 flex flex-col gap-5">
              {view.lessons.map((l, idx) => (
                <LessonEntry key={idx} lesson={l} index={idx} />
              ))}
            </div>
          ) : (
            <p className="mt-4 text-[15px] text-faint">No lessons recorded yet.</p>
          )}

          <h3 className="mt-10 text-[13px] font-medium uppercase tracking-wide text-muted">Plans</h3>
          {view.plans.length ? (
            <ul className="mt-4 flex flex-col gap-2">
              {view.plans.map((p, idx) => (
                <li key={idx} className="text-[13px] text-muted">
                  {p.text}
                  {p.expiresAt && <span className="text-faint"> · expires {p.expiresAt}</span>}
                </li>
              ))}
            </ul>
          ) : (
            <p className="mt-4 text-[15px] text-faint">No standing plans.</p>
          )}

          <h3 className="mt-10 text-[13px] font-medium uppercase tracking-wide text-muted">Setup Matrix</h3>
          <div className="mt-4">
            {loading ? (
              <SkeletonTable rows={4} />
            ) : view.agg.length ? (
              <div className="overflow-x-auto">
                <table className="w-full text-[13px]">
                  <thead>
                    <tr className="border-b border-line text-left text-[11px] uppercase tracking-wide text-muted">
                      <th className="py-2 pr-4 font-normal">symbol</th>
                      <th className="py-2 pr-4 font-normal">session</th>
                      <th className="py-2 pr-4 font-normal">trips</th>
                      <th className="py-2 pr-4 font-normal">w&ndash;l</th>
                      <th className="py-2 font-normal">p/l</th>
                    </tr>
                  </thead>
                  <tbody>
                    {view.agg.map((row, idx) => (
                      <tr key={idx} className="border-b border-line leading-loose">
                        <td className="py-2 pr-4 font-medium text-fg">{row.symbol}</td>
                        <td className="py-2 pr-4 text-muted">{row.session}</td>
                        <td className="py-2 pr-4 tabular-nums">{row.trips}</td>
                        <td className="py-2 pr-4 tabular-nums">{row.wl}</td>
                        <td className="py-2 tabular-nums">{row.plLabel}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            ) : (
              <p className="text-[15px] text-faint">No aggregate data yet.</p>
            )}
          </div>

          <p className="mt-8 text-[11px] tracking-wide text-faint">last dream {view.lastDreamLabel}</p>
        </div>
      </div>
    </div>
  );
}
