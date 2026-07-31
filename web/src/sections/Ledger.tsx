import { Fragment, useState } from 'react';
import { SkeletonTable } from '../components/Skeleton';
import { useReviews, useTrades } from '../lib/hooks';
import { toLedgerRows, type LedgerRow } from '../lib/transform';

const PAGE_SIZE = 30;

function VerdictCell({ verdict }: { verdict: LedgerRow['verdict'] }) {
  if (!verdict) return <span className="text-faint">—</span>;
  const cls = verdict.negative ? 'text-down' : verdict.muted ? 'text-muted' : 'text-up';
  return <span className={cls}>{verdict.glyph}</span>;
}

function LedgerRowView({ row, isOpen, onToggle }: { row: LedgerRow; isOpen: boolean; onToggle: () => void }) {
  const pnlCls = row.pnl === null ? 'text-faint' : row.pnlNegative ? 'text-down' : 'text-up';
  return (
    <Fragment>
      <tr onClick={onToggle} className="cursor-pointer border-b border-line leading-loose hover:bg-white/5">
        <td className="py-2 pr-4 text-muted" title={row.tsTitle}>
          {row.tsLabel}
        </td>
        <td className="py-2 pr-4 font-medium text-fg">{row.symbol}</td>
        <td className="py-2 pr-4 text-muted">{row.action}</td>
        <td className="py-2 pr-4 tabular-nums">{row.qty}</td>
        <td className="py-2 pr-4 tabular-nums">{row.fillLabel}</td>
        <td className={`py-2 pr-4 tabular-nums ${pnlCls}`}>{row.pnlLabel}</td>
        <td className="py-2 pr-4 tracking-wide text-muted">{row.attribution ?? '—'}</td>
        <td className="py-2 pr-4">
          <VerdictCell verdict={row.verdict} />
        </td>
        <td className="py-2 text-muted">{row.exitGrade ?? '—'}</td>
      </tr>
      {isOpen && (
        <tr className="border-b border-line bg-card/60">
          <td />
          <td colSpan={8} className="py-4 pr-6">
            <p className="max-w-[70ch] text-[13px] leading-relaxed text-muted">{row.reason}</p>
          </td>
        </tr>
      )}
    </Fragment>
  );
}

export function Ledger() {
  const trades = useTrades();
  const reviews = useReviews();
  const [expanded, setExpanded] = useState<Set<string | number>>(new Set());
  const [showAll, setShowAll] = useState(false);

  const rows = toLedgerRows(trades.data, reviews.data);
  const visible = showAll ? rows : rows.slice(0, PAGE_SIZE);
  const remaining = rows.length - visible.length;

  function toggle(id: string | number) {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  return (
    <div className="mx-auto max-w-6xl px-6 py-16">
      <h2 className="text-[22px] font-semibold tracking-tight text-fg">The Ledger</h2>
      <div className="mt-6">
        {trades.isLoading ? (
          <SkeletonTable rows={6} />
        ) : rows.length ? (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[880px] text-[13px]">
              <thead>
                <tr className="border-b border-line text-left text-[11px] uppercase tracking-wide text-muted">
                  <th className="py-2 pr-4 font-normal">date</th>
                  <th className="py-2 pr-4 font-normal">symbol</th>
                  <th className="py-2 pr-4 font-normal">action</th>
                  <th className="py-2 pr-4 font-normal">qty</th>
                  <th className="py-2 pr-4 font-normal">fill</th>
                  <th className="py-2 pr-4 font-normal">p&amp;l</th>
                  <th className="py-2 pr-4 font-normal">attribution</th>
                  <th className="py-2 pr-4 font-normal">verdict</th>
                  <th className="py-2 font-normal">exit</th>
                </tr>
              </thead>
              <tbody>
                {visible.map((row) => (
                  <LedgerRowView
                    key={row.id}
                    row={row}
                    isOpen={expanded.has(row.id)}
                    onToggle={() => toggle(row.id)}
                  />
                ))}
              </tbody>
            </table>
            {remaining > 0 && (
              <button
                type="button"
                onClick={() => setShowAll(true)}
                className="mt-4 text-[12px] tracking-wide text-muted underline decoration-line underline-offset-4 hover:text-fg"
              >
                earlier entries ({remaining}) →
              </button>
            )}
          </div>
        ) : (
          <p className="py-6 text-[15px] text-faint">No trades recorded yet.</p>
        )}
      </div>
    </div>
  );
}
