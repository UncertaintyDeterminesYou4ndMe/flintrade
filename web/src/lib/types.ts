// Types mirror the /api/* contract exactly (see dashboard/server.py).
// Every field beyond the required minimum is optional — the backend is a
// best-effort SQLite reader and degrades individual fields to null/absent
// rather than erroring.

export interface RiskState {
  equity?: number;
  day_start_equity?: number;
  day_realized_pnl?: number;
  open_risk?: number;
  halt?: 0 | 1;
  halt_reason?: string;
  updated_at?: string;
}

export interface Position {
  symbol: string;
  side?: string;
  qty?: number;
  entry_price?: number;
  stop?: number;
  target?: number;
  risk_amt?: number;
  source?: string;
  opened_at?: string;
}

export interface Trade {
  id: number | string;
  ts?: string;
  symbol: string;
  action?: string;
  qty?: number;
  fill_price?: number;
  pnl?: number;
  source?: string;
  attribution?: string;
  reason?: string;
}

export interface Intent {
  id: number | string;
  created_at?: string;
  source?: string;
  priority?: number | string;
  symbol: string;
  side?: string;
  status?: string;
  confidence?: number;
  reason?: string;
  reject_reason?: string;
}

export interface ProcessEntry {
  process: string;
  last_beat?: string;
  pid?: number | string;
  status?: string;
  age_sec?: number;
}

export interface EquityPoint {
  ts: string;
  equity: number;
}

export interface EquityCurve {
  inception_date?: string | null;
  inception_equity?: number;
  points: EquityPoint[];
}

export interface TradeReview {
  trade_id: number | string;
  symbol?: string;
  pnl?: number;
  exit_kind?: string;
  slippage_vs_stop?: number;
  confidence?: number;
  thesis_verdict?: string;
  entry_grade?: string;
  exit_grade?: string;
  lesson?: string;
  trade_ts?: string;
}

export interface ConfidenceBucket {
  win_rate?: number;
  n?: number;
}

export interface ReviewStats {
  n?: number;
  win_rate?: number;
  net_pnl?: number;
  avg_slippage_vs_stop?: number;
  exit_kind_counts?: Record<string, number>;
  confidence_buckets?: Record<string, ConfidenceBucket>;
}

export interface ReviewsResponse {
  reviews: TradeReview[];
  stats: ReviewStats;
}

export interface TimeAnchor {
  now_et?: string;
  since_last_dream?: string;
  since_last_user?: string;
  since_last_trade?: string;
}

export interface Lesson {
  text: string;
  confidence: number;
  n?: number;
}

export interface Plan {
  text?: string;
  expires_at?: string;
  tags?: Record<string, unknown>;
  [key: string]: unknown;
}

export interface SelfAssessment {
  n?: number;
  win_rate?: number;
  net_pnl?: number;
  note?: string;
}

export interface Recall {
  time_anchor?: TimeAnchor;
  lessons?: Lesson[];
  plans?: Plan[];
  self_assessment?: SelfAssessment;
}

export interface AggRow {
  symbol?: string;
  session?: string;
  setup?: string;
  trips?: number;
  wins?: number;
  losses?: number;
  pl_ratio?: number;
}

export interface BrainResponse {
  recall?: Recall;
  agg?: AggRow[];
  last_dream_date?: string | null;
}

export interface RiskRules {
  max_risk_pct?: number;
  max_concurrent_positions?: number;
  max_open_risk_pct?: number;
  max_per_symbol_pct?: number;
  max_per_cluster_pct?: number;
  daily_loss_limit_pct?: number;
  volume_ratio_floor?: number;
  session_close_blackout_min?: number;
  revenge_cooldown_min?: number;
  [key: string]: unknown;
}

export interface Config {
  session?: string;
  universe?: string[];
  risk?: RiskRules;
  cadence?: Record<string, number>;
  error?: string;
}
