// ALL api -> view mapping lives here as pure functions. No JSX, no React
// imports. Every function is defensive against null/missing/malformed
// input — the backend is a best-effort SQLite reader that degrades
// individual fields rather than erroring, so the view layer must too.
//
// scripts/smoke.mts exercises every export here against the live backend.

// NOTE: relative imports here carry an explicit ".ts" extension so this
// module can be loaded two ways: bundled by Vite (extension is harmless
// under `allowImportingTsExtensions`) AND executed directly by plain Node
// via `node --experimental-strip-types` in scripts/smoke.mts (which requires
// explicit extensions — Node's ESM resolver does not do extensionless
// lookups the way bundlers do).
import { fmtMoney, fmtMoneyPlain, fmtPercent, parseTs } from './format.ts';
import type {
  RiskState,
  Position,
  Trade,
  Intent,
  ProcessEntry,
  EquityCurve,
  ReviewsResponse,
  TradeReview,
  BrainResponse,
  Config,
  TimeAnchor,
  ConfidenceBucket,
} from './types.ts';

// ─────────────────────────────────────────────────────────────────────────
// small local helpers (kept private — not part of the public transform API)
// ─────────────────────────────────────────────────────────────────────────

function two(n: number): string {
  return n < 10 ? `0${n}` : `${n}`;
}

/** number | null | undefined -> number | null, NaN-safe. */
function num(v: number | null | undefined): number | null {
  return typeof v === 'number' && !Number.isNaN(v) ? v : null;
}

/** "MM-DD HH:mm" in the viewer's local time; '' if ts is unparseable. */
function shortDateTime(ts: string | null | undefined): string {
  const d = parseTs(ts);
  if (!d) return '—';
  return `${two(d.getMonth() + 1)}-${two(d.getDate())} ${two(d.getHours())}:${two(d.getMinutes())}`;
}

/** Full ISO string for a title="" attribute; '' if unparseable. */
function isoTitle(ts: string | null | undefined): string {
  const d = parseTs(ts);
  if (!d) return '';
  return d.toISOString();
}

function tsMillis(ts: string | null | undefined): number {
  const d = parseTs(ts);
  return d ? d.getTime() : 0;
}

function ageLabel(ageSec: number | null | undefined): string {
  if (ageSec === null || ageSec === undefined || Number.isNaN(ageSec)) return '—';
  const s = Math.max(0, Math.round(ageSec));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h`;
  const dd = Math.floor(h / 24);
  return `${dd}d`;
}

// ─────────────────────────────────────────────────────────────────────────
// § The Equity Line
// ─────────────────────────────────────────────────────────────────────────

export interface EquityViewPoint {
  ts: string;
  label: string;
  equity: number;
}

export interface EquityView {
  points: EquityViewPoint[];
  hasData: boolean;
  inceptionEquity: number | null;
  lastEquity: number | null;
  lastIsUp: boolean;
  returnPct: number | null;
  returnLabel: string;
  closedTrades: number;
  summaryLine: string;
}

export function toEquityView(curve: EquityCurve | null | undefined, risk: RiskState | null | undefined): EquityView {
  const rawPoints = Array.isArray(curve?.points) ? curve!.points : [];
  const points: EquityViewPoint[] = rawPoints
    .filter((p) => p && typeof p.equity === 'number' && !Number.isNaN(p.equity) && p.ts)
    .map((p) => ({ ts: p.ts, label: shortDateTime(p.ts), equity: p.equity }));

  const inceptionEquity =
    typeof curve?.inception_equity === 'number' && !Number.isNaN(curve.inception_equity)
      ? curve.inception_equity
      : null;

  const lastPoint = points.length ? points[points.length - 1] : undefined;
  const lastEquity =
    typeof risk?.equity === 'number' && !Number.isNaN(risk.equity) ? risk.equity : (lastPoint?.equity ?? null);

  const returnPct =
    inceptionEquity !== null && inceptionEquity !== 0 && lastEquity !== null
      ? ((lastEquity - inceptionEquity) / inceptionEquity) * 100
      : null;

  return {
    points,
    hasData: points.length > 0,
    inceptionEquity,
    lastEquity,
    lastIsUp: (returnPct ?? 0) >= 0,
    returnPct,
    returnLabel: fmtPercent(returnPct, { signed: true }),
    closedTrades: points.length,
    summaryLine: `return since inception ${fmtPercent(returnPct, { signed: true })} · ${points.length} closed trade${points.length === 1 ? '' : 's'}`,
  };
}

// ─────────────────────────────────────────────────────────────────────────
// § The Ledger (trades, joined with reviews)
// ─────────────────────────────────────────────────────────────────────────

const ATTRIBUTION_LABEL: Record<string, string> = {
  'outage-degraded': 'outage',
};

const VERDICT_GLYPH: Record<string, { glyph: string; negative: boolean }> = {
  correct: { glyph: '✓ correct', negative: false },
  wrong: { glyph: '✗ wrong', negative: true },
  partial: { glyph: '◐ partial', negative: false },
};

export interface LedgerRow {
  id: string | number;
  tsLabel: string;
  tsTitle: string;
  tsMillis: number;
  symbol: string;
  action: string;
  qty: string;
  fillLabel: string;
  pnl: number | null;
  pnlLabel: string;
  pnlNegative: boolean;
  attribution: string | null;
  verdict: { glyph: string; negative: boolean; muted: boolean } | null;
  exitGrade: string | null;
  reason: string;
}

export function toLedgerRows(trades: Trade[] | null | undefined, reviews: ReviewsResponse | null | undefined): LedgerRow[] {
  const list = Array.isArray(trades) ? trades : [];
  const reviewByTradeId = new Map<string | number, TradeReview>();
  for (const r of reviews?.reviews ?? []) {
    if (r && r.trade_id !== undefined && r.trade_id !== null) reviewByTradeId.set(r.trade_id, r);
  }

  const rows: LedgerRow[] = list.map((t) => {
    const review = reviewByTradeId.get(t.id);
    const pnl = typeof t.pnl === 'number' && !Number.isNaN(t.pnl) ? t.pnl : null;
    const rawAttribution = t.attribution ?? null;
    const verdictKey = review?.thesis_verdict ?? null;
    const verdictMeta = verdictKey ? VERDICT_GLYPH[verdictKey] : undefined;
    return {
      id: t.id,
      tsLabel: shortDateTime(t.ts),
      tsTitle: isoTitle(t.ts),
      tsMillis: tsMillis(t.ts),
      symbol: t.symbol ?? '—',
      action: t.action ?? '—',
      qty: t.qty !== undefined && t.qty !== null ? String(t.qty) : '—',
      fillLabel: t.fill_price !== undefined && t.fill_price !== null ? fmtMoneyPlain(t.fill_price) : '—',
      pnl,
      pnlLabel: pnl !== null ? fmtMoney(pnl) : '—',
      pnlNegative: (pnl ?? 0) < 0,
      attribution: rawAttribution ? (ATTRIBUTION_LABEL[rawAttribution] ?? rawAttribution) : null,
      verdict: verdictMeta
        ? { glyph: verdictMeta.glyph, negative: verdictMeta.negative, muted: verdictKey === 'partial' }
        : verdictKey
          ? { glyph: verdictKey, negative: false, muted: true }
          : null,
      exitGrade: review?.exit_grade ?? null,
      reason: t.reason?.trim() || 'no reasoning recorded for this trade.',
    };
  });

  return rows.sort((a, b) => b.tsMillis - a.tsMillis);
}

// ─────────────────────────────────────────────────────────────────────────
// § Order Blotter (intents)
// ─────────────────────────────────────────────────────────────────────────

export interface BlotterRow {
  id: string | number;
  timeLabel: string;
  timeTitle: string;
  timeMillis: number;
  source: string;
  side: string;
  symbol: string;
  confLabel: string;
  status: string;
  pending: boolean;
  rejectReason: string | null;
}

export function toBlotterRows(intents: Intent[] | null | undefined): BlotterRow[] {
  const list = Array.isArray(intents) ? intents : [];
  const rows: BlotterRow[] = list.map((i) => {
    const status = (i.status ?? '—').toString();
    return {
      id: i.id,
      timeLabel: shortDateTime(i.created_at),
      timeTitle: isoTitle(i.created_at),
      timeMillis: tsMillis(i.created_at),
      source: i.source ?? '—',
      side: i.side ?? '—',
      symbol: i.symbol ?? '—',
      confLabel:
        i.confidence !== undefined && i.confidence !== null && !Number.isNaN(i.confidence)
          ? `${Math.round(i.confidence)}%`
          : '—',
      status,
      pending: status.toLowerCase() === 'pending',
      rejectReason: i.reject_reason?.trim() || null,
    };
  });
  return rows.sort((a, b) => b.timeMillis - a.timeMillis);
}

// ─────────────────────────────────────────────────────────────────────────
// § Dream Journal (brain + reviews.stats)
// ─────────────────────────────────────────────────────────────────────────

export interface CalibrationRow {
  bucket: string;
  hasData: boolean;
  winRatePct: number | null;
  n: number;
  barPct: number;
  belowHalf: boolean;
}

export interface LessonRow {
  text: string;
  confidence: number;
  n: number | null;
  strong: boolean;
  barPct: number;
}

export interface PlanRow {
  text: string;
  expiresAt: string | null;
}

export interface AggMatrixRow {
  symbol: string;
  session: string;
  trips: string;
  wl: string;
  plLabel: string;
}

export interface DreamView {
  winRatePct: number | null;
  winRateLabel: string;
  n: number | null;
  netPnl: number | null;
  netPnlLabel: string;
  netPnlNegative: boolean;
  note: string | null;
  hasSelfAssessment: boolean;
  calibration: CalibrationRow[];
  lessons: LessonRow[];
  plans: PlanRow[];
  agg: AggMatrixRow[];
  lastDreamLabel: string;
  timeAnchorLine: string | null;
}

// Fixed display order — the buckets object is keyed exactly "<60"/"60-70"/">70".
const CONFIDENCE_BUCKET_ORDER = ['<60', '60-70', '>70'] as const;

export function toDreamView(brain: BrainResponse | null | undefined, reviews: ReviewsResponse | null | undefined): DreamView {
  const recall = brain?.recall;
  const sa = recall?.self_assessment;
  const winRatePct = typeof sa?.win_rate === 'number' && !Number.isNaN(sa.win_rate) ? sa.win_rate * 100 : null;
  const netPnl = typeof sa?.net_pnl === 'number' && !Number.isNaN(sa.net_pnl) ? sa.net_pnl : null;

  const buckets: Record<string, ConfidenceBucket> = reviews?.stats?.confidence_buckets ?? {};
  const calibration: CalibrationRow[] = CONFIDENCE_BUCKET_ORDER.map((bucket) => {
    const b = buckets[bucket];
    const winRate = typeof b?.win_rate === 'number' && !Number.isNaN(b.win_rate) ? b.win_rate : null;
    const n = typeof b?.n === 'number' && !Number.isNaN(b.n) ? b.n : 0;
    return {
      bucket,
      hasData: winRate !== null,
      winRatePct: winRate !== null ? winRate * 100 : null,
      n,
      barPct: winRate !== null ? Math.max(0, Math.min(100, winRate * 100)) : 0,
      belowHalf: winRate !== null && winRate < 0.5,
    };
  });

  const lessons: LessonRow[] = (Array.isArray(recall?.lessons) ? recall!.lessons : [])
    .filter((l) => l && typeof l.text === 'string')
    .map((l) => {
      const confidence = typeof l.confidence === 'number' && !Number.isNaN(l.confidence) ? l.confidence : 0;
      return {
        text: l.text,
        confidence,
        n: typeof l.n === 'number' ? l.n : null,
        strong: confidence >= 0.8,
        barPct: Math.max(0, Math.min(100, confidence * 100)),
      };
    });

  const plans: PlanRow[] = (Array.isArray(recall?.plans) ? recall!.plans : [])
    .filter((p) => p !== null && p !== undefined)
    .map((p) => ({
      text: typeof p.text === 'string' && p.text ? p.text : JSON.stringify(p),
      expiresAt: typeof p.expires_at === 'string' ? p.expires_at : null,
    }));

  const agg: AggMatrixRow[] = (Array.isArray(brain?.agg) ? brain!.agg : []).map((row) => ({
    symbol: row?.symbol ?? '—',
    session: row?.session ?? '—',
    trips: row?.trips !== undefined && row?.trips !== null ? String(row.trips) : '—',
    wl: `${row?.wins ?? 0}–${row?.losses ?? 0}`,
    plLabel: typeof row?.pl_ratio === 'number' && !Number.isNaN(row.pl_ratio) ? row.pl_ratio.toFixed(2) : '—',
  }));

  return {
    winRatePct,
    winRateLabel: fmtPercent(winRatePct),
    n: typeof sa?.n === 'number' ? sa.n : null,
    netPnl,
    netPnlLabel: netPnl !== null ? fmtMoney(netPnl) : '—',
    netPnlNegative: (netPnl ?? 0) < 0,
    note: sa?.note ?? null,
    hasSelfAssessment: !!sa,
    calibration,
    lessons,
    plans,
    agg,
    lastDreamLabel: brain?.last_dream_date ?? 'never',
    timeAnchorLine: timeAnchorLine(recall?.time_anchor),
  };
}

function timeAnchorLine(anchor: TimeAnchor | null | undefined): string | null {
  if (!anchor) return null;
  const parts: string[] = [];
  if (anchor.since_last_dream) parts.push(`last dream ${anchor.since_last_dream} ago`);
  if (anchor.since_last_trade) parts.push(`last trade ${anchor.since_last_trade} ago`);
  if (anchor.since_last_user) parts.push(`last user touch ${anchor.since_last_user} ago`);
  return parts.length ? parts.join(' · ') : null;
}

// ─────────────────────────────────────────────────────────────────────────
// § The Desk (risk + processes + time anchor)
// ─────────────────────────────────────────────────────────────────────────

export interface WireRow {
  name: string;
  ageLabel: string;
  freshness: 'fresh' | 'stale' | 'dead';
}

export interface DeskView {
  equityLabel: string;
  dayPnlLabel: string;
  dayPnlNegative: boolean;
  openRiskLabel: string;
  session: string;
  wire: WireRow[];
  timeAnchorLine: string | null;
}

export function toDeskView(
  risk: RiskState | null | undefined,
  processes: ProcessEntry[] | null | undefined,
  timeAnchor: TimeAnchor | null | undefined,
  session?: string | null,
): DeskView {
  const day = typeof risk?.day_realized_pnl === 'number' && !Number.isNaN(risk.day_realized_pnl) ? risk.day_realized_pnl : null;
  const wire: WireRow[] = (Array.isArray(processes) ? processes : []).map((p) => {
    let ageSec: number | null | undefined = p.age_sec;
    if (ageSec === undefined || ageSec === null) {
      const d = parseTs(p.last_beat);
      ageSec = d ? (Date.now() - d.getTime()) / 1000 : null;
    }
    let freshness: WireRow['freshness'] = 'dead';
    if (ageSec !== null && ageSec !== undefined && !Number.isNaN(ageSec)) {
      if (ageSec < 120) freshness = 'fresh';
      else if (ageSec < 600) freshness = 'stale';
      else freshness = 'dead';
    }
    return { name: p.process, ageLabel: ageLabel(ageSec), freshness };
  });

  return {
    equityLabel: fmtMoneyPlain(risk?.equity),
    dayPnlLabel: fmtMoney(day),
    dayPnlNegative: (day ?? 0) < 0,
    openRiskLabel: fmtMoneyPlain(risk?.open_risk),
    session: session ?? '—',
    wire,
    timeAnchorLine: timeAnchorLine(timeAnchor),
  };
}

// ─────────────────────────────────────────────────────────────────────────
// § Colophon (config)
// ─────────────────────────────────────────────────────────────────────────

export interface LabelValue {
  label: string;
  value: string;
}

export interface ColophonView {
  houseRules: LabelValue[];
  universe: string[];
  cadence: LabelValue[];
  session: string;
  hasError: boolean;
}

const RISK_RULE_LABELS: [string, string, string][] = [
  ['max_risk_pct', 'max risk per trade', '%'],
  ['max_concurrent_positions', 'concurrent positions', ''],
  ['max_open_risk_pct', 'max open risk', '%'],
  ['max_per_symbol_pct', 'max per symbol', '%'],
  ['max_per_cluster_pct', 'max per cluster', '%'],
  ['daily_loss_limit_pct', 'daily circuit breaker', '%'],
  ['volume_ratio_floor', 'volume ratio floor', ''],
  ['session_close_blackout_min', 'session-close blackout', 'm'],
  ['revenge_cooldown_min', 'revenge cooldown', 'm'],
];

export function toColophon(config: Config | null | undefined): ColophonView {
  const risk = config?.risk ?? {};
  const houseRules: LabelValue[] = RISK_RULE_LABELS.filter(([key]) => risk[key] !== undefined && risk[key] !== null).map(
    ([key, label, unit]) => ({ label, value: `${risk[key]}${unit}` }),
  );

  const cadenceEntries = config?.cadence ?? {};
  const cadence: LabelValue[] = Object.entries(cadenceEntries)
    .filter(([, v]) => typeof v === 'number' && !Number.isNaN(v))
    .map(([key, v]) => ({
      label: key.replace(/_sec$/, '').replace(/_/g, ' '),
      value: `${v}s`,
    }));

  return {
    houseRules,
    universe: Array.isArray(config?.universe) ? config!.universe : [],
    cadence,
    session: config?.session ?? '—',
    hasError: !!config?.error,
  };
}

// ─────────────────────────────────────────────────────────────────────────
// § Front page — open positions ledger
// ─────────────────────────────────────────────────────────────────────────

export interface PositionRow {
  symbol: string;
  side: string;
  /** true = long, false = short, null = side unknown. */
  sideLong: boolean | null;
  qty: string;
  entryLabel: string;
  stopLabel: string;
  targetLabel: string;
  riskLabel: string;
  /** "1 : 2.4" reward-to-risk, null when it can't be derived. */
  rrLabel: string | null;
  /** stop/entry/target mapped to 0–100% along the stop→target range, for the range bar. */
  bar: { stopPct: number; entryPct: number; targetPct: number } | null;
  openedLabel: string;
  openedTitle: string;
}

export function toPositionRows(positions: Position[] | null | undefined): PositionRow[] {
  const list = Array.isArray(positions) ? positions : [];
  return list.map((p) => {
    const entry = num(p.entry_price);
    const stop = num(p.stop);
    const target = num(p.target);

    // R-multiple and the range bar both need all three prices.
    let rrLabel: string | null = null;
    let bar: PositionRow['bar'] = null;
    if (entry !== null && stop !== null && target !== null && entry !== stop) {
      const riskPerShare = Math.abs(entry - stop);
      const rewardPerShare = Math.abs(target - entry);
      if (riskPerShare > 0 && rewardPerShare > 0) {
        rrLabel = `1 : ${(rewardPerShare / riskPerShare).toFixed(1)}`;
      }
      const lo = Math.min(stop, target);
      const hi = Math.max(stop, target);
      if (hi > lo) {
        const pct = (v: number) => Math.round(((v - lo) / (hi - lo)) * 1000) / 10;
        bar = { stopPct: pct(stop), entryPct: pct(entry), targetPct: pct(target) };
      }
    }

    const sideNorm = (p.side ?? '').toString().toLowerCase();
    const sideLong = !sideNorm ? null : /long|buy/.test(sideNorm) ? true : /short|sell/.test(sideNorm) ? false : null;

    return {
      symbol: p.symbol ?? '—',
      side: p.side ?? '—',
      sideLong,
      qty: p.qty !== undefined && p.qty !== null ? String(p.qty) : '—',
      entryLabel: p.entry_price !== undefined && p.entry_price !== null ? fmtMoneyPlain(p.entry_price) : '—',
      stopLabel: p.stop !== undefined && p.stop !== null ? fmtMoneyPlain(p.stop) : '—',
      targetLabel: p.target !== undefined && p.target !== null ? fmtMoneyPlain(p.target) : '—',
      riskLabel: p.risk_amt !== undefined && p.risk_amt !== null ? fmtMoneyPlain(p.risk_amt) : '—',
      rrLabel,
      bar,
      openedLabel: shortDateTime(p.opened_at),
      openedTitle: isoTitle(p.opened_at),
    };
  });
}

// ─────────────────────────────────────────────────────────────────────────
// § Front page — glanceable stat cards
// ─────────────────────────────────────────────────────────────────────────

export type Tone = 'up' | 'down' | 'flat';

export interface FrontStats {
  dayPnlLabel: string;
  dayPnlPctLabel: string;
  dayTone: Tone;
  returnLabel: string;
  returnTone: Tone;
  openRiskLabel: string;
  winRateLabel: string;
  winRateMeta: string | null;
}

function toneOf(v: number | null): Tone {
  if (v === null || v === 0) return 'flat';
  return v > 0 ? 'up' : 'down';
}

export function toFrontStats(
  risk: RiskState | null | undefined,
  curve: EquityCurve | null | undefined,
  brain: BrainResponse | null | undefined,
): FrontStats {
  const day = num(risk?.day_realized_pnl);
  const dayStart = num(risk?.day_start_equity);
  const dayPct = day !== null && dayStart ? (day / dayStart) * 100 : null;

  const inception = num(curve?.inception_equity);
  const points = Array.isArray(curve?.points) ? curve!.points : [];
  const lastEquity = num(risk?.equity) ?? (points.length ? num(points[points.length - 1]?.equity) : null);
  const returnPct =
    inception !== null && inception !== 0 && lastEquity !== null
      ? ((lastEquity - inception) / inception) * 100
      : null;

  const sa = brain?.recall?.self_assessment;
  const winRate = typeof sa?.win_rate === 'number' && !Number.isNaN(sa.win_rate) ? sa.win_rate * 100 : null;

  return {
    dayPnlLabel: fmtMoney(day),
    dayPnlPctLabel: fmtPercent(dayPct, { signed: true }),
    dayTone: toneOf(day),
    returnLabel: fmtPercent(returnPct, { signed: true }),
    returnTone: toneOf(returnPct),
    openRiskLabel: fmtMoneyPlain(risk?.open_risk),
    winRateLabel: fmtPercent(winRate),
    winRateMeta: typeof sa?.n === 'number' ? `n=${sa.n}` : null,
  };
}
