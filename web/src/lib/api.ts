import type {
  RiskState,
  Position,
  Trade,
  Intent,
  ProcessEntry,
  EquityCurve,
  ReviewsResponse,
  BrainResponse,
  Config,
} from './types';

async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(path);
  if (!res.ok) {
    throw new Error(`${path} -> HTTP ${res.status}`);
  }
  return (await res.json()) as T;
}

export const api = {
  risk: () => getJSON<RiskState>('/api/risk'),
  positions: () => getJSON<Position[]>('/api/positions'),
  trades: () => getJSON<Trade[]>('/api/trades'),
  intents: () => getJSON<Intent[]>('/api/intents'),
  processes: () => getJSON<ProcessEntry[]>('/api/processes'),
  equityCurve: () => getJSON<EquityCurve>('/api/equity-curve'),
  reviews: () => getJSON<ReviewsResponse>('/api/reviews'),
  brain: () => getJSON<BrainResponse>('/api/brain'),
  config: () => getJSON<Config>('/api/config'),
};

export const FAST_MS = 5_000;
export const SLOW_MS = 30_000;
