import { Fragment, useState } from 'react';
import { SkeletonTable } from '../components/Skeleton';
import { useIntents } from '../lib/hooks';
import { toBlotterRows, type BlotterRow } from '../lib/transform';

const PAGE_SIZE = 30;

function StatusCell({ row }: { row: BlotterRow }) {
  return (
    <span className="inline-flex items-center gap-1.5">
      {row.pending && <span className="dot-breathe inline-block h-1.5 w-1.5 rounded-full bg-up" />}
      <span className="text-muted">{row.status}</span>
    </span>
  );
}

function BlotterRowView({ row }: { row: BlotterRow }) {
  return (
    <Fragment>
      <tr className="border-b border-line leading-loose">
        <td className="py-2 pr-4 text-muted" title={row.timeTitle}>
          {row.timeLabel}
        </td>
        <td className="py-2 pr-4 tracking-wide text-muted">{row.source}</td>
        <td className="py-2 pr-4 text-muted">{row.side}</td>
        <td className="py-2 pr-4 font-medium text-fg">{row.symbol}</td>
        <td className="py-2 pr-4 tabular-nums">{row.confLabel}</td>
        <td className="py-2">
          <StatusCell row={row} />
        </td>
      </tr>
      {row.rejectReason && (
        <tr className="border-b border-line">
          <td />
          <td colSpan={5} className="pb-2 pt-0 text-xs text-down">
            ── {row.rejectReason}
          </td>
        </tr>
      )}
    </Fragment>
  );
}

export function Blotter() {
  const intents = useIntents();
  const [showAll, setShowAll] = useState(false);

  const rows = toBlotterRows(intents.data);
  const visible = showAll ? rows : rows.slice(0, PAGE_SIZE);
  const remaining = rows.length - visible.length;

  return (
    <div className="mx-auto max-w-6xl px-6 py-16">
      <h2 className="text-[22px] font-semibold tracking-tight text-fg">Order Blotter</h2>
      <div className="mt-6">
        {intents.isLoading ? (
          <SkeletonTable rows={6} />
        ) : rows.length ? (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[720px] text-[13px]">
              <thead>
                <tr className="border-b border-line text-left text-[11px] uppercase tracking-wide text-muted">
                  <th className="py-2 pr-4 font-normal">time</th>
                  <th className="py-2 pr-4 font-normal">source</th>
                  <th className="py-2 pr-4 font-normal">side</th>
                  <th className="py-2 pr-4 font-normal">symbol</th>
                  <th className="py-2 pr-4 font-normal">conf</th>
                  <th className="py-2 font-normal">status</th>
                </tr>
              </thead>
              <tbody>
                {visible.map((row) => (
                  <BlotterRowView key={row.id} row={row} />
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
          <p className="py-6 text-[15px] text-faint">No intents recorded yet.</p>
        )}
      </div>
    </div>
  );
}
