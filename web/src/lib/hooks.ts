import { useQuery } from '@tanstack/react-query';
import { api, FAST_MS, SLOW_MS } from './api';

// Fast-polling queries (5s) — the "is something happening right now" surfaces.
export function useRisk() {
  return useQuery({ queryKey: ['risk'], queryFn: api.risk, refetchInterval: FAST_MS });
}
export function useProcesses() {
  return useQuery({ queryKey: ['processes'], queryFn: api.processes, refetchInterval: FAST_MS });
}
export function useIntents() {
  return useQuery({ queryKey: ['intents'], queryFn: api.intents, refetchInterval: FAST_MS });
}

// Slow-polling queries (30s) — historical / aggregate surfaces.
export function usePositions() {
  return useQuery({ queryKey: ['positions'], queryFn: api.positions, refetchInterval: SLOW_MS });
}
export function useTrades() {
  return useQuery({ queryKey: ['trades'], queryFn: api.trades, refetchInterval: SLOW_MS });
}
export function useEquityCurve() {
  return useQuery({ queryKey: ['equity-curve'], queryFn: api.equityCurve, refetchInterval: SLOW_MS });
}
export function useReviews() {
  return useQuery({ queryKey: ['reviews'], queryFn: api.reviews, refetchInterval: SLOW_MS });
}
export function useBrain() {
  return useQuery({ queryKey: ['brain'], queryFn: api.brain, refetchInterval: SLOW_MS });
}
export function useConfig() {
  return useQuery({ queryKey: ['config'], queryFn: api.config, refetchInterval: SLOW_MS });
}

/** True if any of the given react-query results is in an error state — drives the offline banner. */
export function anyError(...results: Array<{ isError: boolean }>): boolean {
  return results.some((r) => r.isError);
}
