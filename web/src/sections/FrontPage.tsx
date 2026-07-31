import type { ReactNode } from 'react';
import { EquityChart } from '../components/EquityChart';
import { SkeletonBlock, SkeletonLines } from '../components/Skeleton';
import { useRisk, useEquityCurve, usePositions, useProcesses, useBrain } from '../lib/hooks';
import {
  toEquityView,
  toDeskView,
  toPositionRows,
  toFrontStats,
  type PositionRow,
  type Tone,
  type WireRow as WireRowData,
} from '../lib/transform';

const TONE_TEXT: Record<Tone, string> = { up: 'text-up', down: 'text-down', flat: 'text-muted' };
const TONE_PILL: Record<Tone, string> = {
  up: 'bg-up/15 text-up',
  down: 'bg-down/15 text-down',
  flat: 'bg-card text-muted',
};

function DeltaPill({ tone, children }: { tone: Tone; children: ReactNode }) {
  return (
    <span
      className={`inline-flex items-center gap-1 rounded-full px-2.5 py-1 text-[13px] font-semibold tabular-nums ${TONE_PILL[tone]}`}
    >
      {children}
    </span>
  );
}

function StatCard({
  label,
  value,
  tone,
  meta,
}: {
  label: string;
  value: string;
  tone?: Tone;
  meta?: string | null;
}) {
  return (
    <div className="rounded-2xl bg-card p-4">
      <p className="text-[12px] font-medium text-muted">{label}</p>
      <p
        className={`mt-1.5 text-[22px] font-semibold leading-none tabular-nums ${tone ? TONE_TEXT[tone] : 'text-fg'}`}
      >
        {value}
      </p>
      {meta && <p className="mt-1.5 text-[11px] text-faint">{meta}</p>}
    </div>
  );
}

/** stop → entry → target as a red/green range bar — the trade's shape at a glance. */
function RangeBar({ row }: { row: PositionRow }) {
  const bar = row.bar!;
  const redL = Math.min(bar.entryPct, bar.stopPct);
  const redW = Math.abs(bar.entryPct - bar.stopPct);
  const greenL = Math.min(bar.entryPct, bar.targetPct);
  const greenW = Math.abs(bar.entryPct - bar.targetPct);
  // labels follow the bar's direction: for shorts the stop sits on the right
  const stopOnLeft = bar.stopPct <= bar.targetPct;
  return (
    <div className="mt-3">
      <div className="relative h-1.5 w-full rounded-full bg-line">
        <div
          className="absolute inset-y-0 rounded-full bg-down/50"
          style={{ left: `${redL}%`, width: `${redW}%` }}
        />
        <div
          className="absolute inset-y-0 rounded-full bg-up/50"
          style={{ left: `${greenL}%`, width: `${greenW}%` }}
        />
        <div
          className="absolute top-1/2 h-2.5 w-2.5 -translate-x-1/2 -translate-y-1/2 rounded-full bg-fg"
          style={{ left: `${bar.entryPct}%` }}
        />
      </div>
      <div className="mt-1.5 flex items-baseline justify-between text-[11px] tabular-nums">
        <span className={stopOnLeft ? 'text-down' : 'text-up'}>
          {stopOnLeft ? `stop ${row.stopLabel}` : `target ${row.targetLabel}`}
        </span>
        <span className="text-faint">
          risk {row.riskLabel}
          {row.rrLabel ? ` · R ${row.rrLabel}` : ''}
        </span>
        <span className={stopOnLeft ? 'text-up' : 'text-down'}>
          {stopOnLeft ? `target ${row.targetLabel}` : `stop ${row.stopLabel}`}
        </span>
      </div>
    </div>
  );
}

function PositionCard({ row }: { row: PositionRow }) {
  return (
    <div className="border-b border-line px-4 py-3.5 last:border-0">
      <div className="flex items-center gap-2.5">
        <p className="text-[16px] font-semibold text-fg">{row.symbol}</p>
        {row.sideLong !== null && (
          <span
            className={`rounded-md px-1.5 py-0.5 text-[11px] font-semibold uppercase ${
              row.sideLong ? 'bg-up/15 text-up' : 'bg-down/15 text-down'
            }`}
          >
            {row.side}
          </span>
        )}
        <span className="text-[13px] tabular-nums text-muted">×{row.qty}</span>
        <div className="ml-auto text-right">
          <p className="text-[15px] tabular-nums text-fg">{row.entryLabel}</p>
          <p className="text-[11px] text-faint" title={row.openedTitle}>
            entry · {row.openedLabel}
          </p>
        </div>
      </div>
      {row.bar ? (
        <RangeBar row={row} />
      ) : (
        <div className="mt-2 flex flex-wrap gap-x-4 text-[12px] tabular-nums text-muted">
          <span>stop {row.stopLabel}</span>
          <span>target {row.targetLabel}</span>
          <span>risk {row.riskLabel}</span>
        </div>
      )}
    </div>
  );
}

function WireRow({ row }: { row: WireRowData }) {
  const dotClass =
    row.freshness === 'fresh' ? 'bg-up dot-breathe' : row.freshness === 'stale' ? 'bg-faint' : 'bg-down';
  return (
    <div className="flex items-center gap-2 py-1 text-[12px]">
      <span className={`h-1.5 w-1.5 shrink-0 rounded-full ${dotClass}`} />
      <span className="text-muted">{row.name}</span>
      <span className="ml-auto tabular-nums text-faint">{row.ageLabel}</span>
    </div>
  );
}

export function FrontPage() {
  const risk = useRisk();
  const equityCurve = useEquityCurve();
  const positions = usePositions();
  const processes = useProcesses();
  const brain = useBrain();

  const equityView = toEquityView(equityCurve.data, risk.data);
  const deskView = toDeskView(risk.data, processes.data, brain.data?.recall?.time_anchor);
  const positionRows = toPositionRows(positions.data);
  const stats = toFrontStats(risk.data, equityCurve.data, brain.data);

  const dayArrow = stats.dayTone === 'up' ? '▲' : stats.dayTone === 'down' ? '▼' : null;

  return (
    <div className="mx-auto max-w-6xl px-6 pb-16 pt-6">
      {/* ── hero: portfolio value, Apple Stocks style ── */}
      <section>
        <p className="text-[13px] font-medium text-muted">Equity</p>
        <div className="mt-1 flex flex-wrap items-center gap-x-4 gap-y-2">
          <p className="text-[42px] font-bold leading-none tracking-tight tabular-nums text-fg">
            {deskView.equityLabel}
          </p>
          <DeltaPill tone={stats.dayTone}>
            {dayArrow && <span className="text-[10px]">{dayArrow}</span>}
            {stats.dayPnlLabel}
            {stats.dayPnlPctLabel !== '—' && <span className="font-normal">({stats.dayPnlPctLabel})</span>}
          </DeltaPill>
        </div>
        <p className="mt-2 text-[13px] text-muted">
          return since inception <span className={TONE_TEXT[stats.returnTone]}>{stats.returnLabel}</span>
          {' · '}
          {equityView.closedTrades} closed trade{equityView.closedTrades === 1 ? '' : 's'}
        </p>
      </section>

      {/* ── equity curve, tinted by the period's result ── */}
      <div className="mt-6 rounded-2xl bg-card p-3 sm:p-4">
        {equityCurve.isLoading ? <SkeletonBlock height={256} /> : <EquityChart view={equityView} />}
      </div>

      {/* ── glanceable stat cards ── */}
      <div className="mt-6 grid grid-cols-2 gap-3 lg:grid-cols-4">
        <StatCard
          label="Day P&L"
          value={stats.dayPnlLabel}
          tone={stats.dayTone}
          meta={stats.dayPnlPctLabel !== '—' ? stats.dayPnlPctLabel : null}
        />
        <StatCard label="Total Return" value={stats.returnLabel} tone={stats.returnTone} meta="since inception" />
        <StatCard
          label="Open Risk"
          value={stats.openRiskLabel}
          meta={`${positionRows.length} position${positionRows.length === 1 ? '' : 's'}`}
        />
        <StatCard label="Win Rate" value={stats.winRateLabel} meta={stats.winRateMeta} />
      </div>

      {/* ── open positions ── */}
      <section className="mt-10">
        <div className="flex items-baseline justify-between">
          <h2 className="text-[20px] font-semibold tracking-tight text-fg">Open Positions</h2>
          <p className="text-[12px] text-muted">
            {positionRows.length} open · risk {stats.openRiskLabel}
          </p>
        </div>
        <div className="mt-3 overflow-hidden rounded-2xl bg-card">
          {positions.isLoading ? (
            <div className="p-4">
              <SkeletonLines rows={3} />
            </div>
          ) : positionRows.length ? (
            positionRows.map((p, idx) => <PositionCard key={`${p.symbol}-${idx}`} row={p} />)
          ) : (
            <div className="px-4 py-10 text-center">
              <p className="text-[15px] text-faint">No open positions</p>
              <p className="mt-1 text-[13px] text-faint">Cash is a position.</p>
            </div>
          )}
        </div>
      </section>

      {/* ── wire status ── */}
      <section className="mt-10">
        <h2 className="text-[13px] font-medium uppercase tracking-wide text-muted">Wire Status</h2>
        <div className="mt-2 grid grid-cols-1 gap-x-10 sm:grid-cols-2 lg:grid-cols-4">
          {processes.isLoading ? (
            <SkeletonLines rows={4} />
          ) : deskView.wire.length ? (
            deskView.wire.map((w) => <WireRow key={w.name} row={w} />)
          ) : (
            <p className="text-[12px] text-faint">no process data</p>
          )}
        </div>
        {deskView.timeAnchorLine && <p className="mt-4 text-[12px] text-faint">{deskView.timeAnchorLine}</p>}
      </section>
    </div>
  );
}
