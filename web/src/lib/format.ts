// Formatting helpers shared across the app (mainly by lib/transform.ts).
// Everything is defensive: bad or missing input renders a dash rather than
// throwing or printing "NaN".

export function fmtMoney(value: number | null | undefined): string {
  if (value === null || value === undefined || Number.isNaN(value)) return '—';
  const sign = value < 0 ? '-' : '+';
  const abs = Math.abs(value);
  return `${sign}$${abs.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}

/** Money without a forced sign — for absolute magnitudes like equity. */
export function fmtMoneyPlain(value: number | null | undefined): string {
  if (value === null || value === undefined || Number.isNaN(value)) return '—';
  const sign = value < 0 ? '-' : '';
  const abs = Math.abs(value);
  return `${sign}$${abs.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}

export function fmtPercent(value: number | null | undefined, opts?: { signed?: boolean }): string {
  if (value === null || value === undefined || Number.isNaN(value)) return '—';
  const sign = opts?.signed && value > 0 ? '+' : '';
  return `${sign}${value.toFixed(1)}%`;
}

/** Parse a timestamp that may be a UTC 'YYYY-MM-DDTHH:MM:SSZ' string, an ISO
 * string with offset, or a bare 'YYYY-MM-DD HH:MM:SS' (assumed UTC). */
export function parseTs(ts: string | null | undefined): Date | null {
  if (!ts) return null;
  let s = ts.trim();
  if (!s) return null;
  // Bare "YYYY-MM-DD HH:MM:SS" (no timezone) — treat as UTC.
  if (/^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}$/.test(s)) {
    s = s.replace(' ', 'T') + 'Z';
  }
  const d = new Date(s);
  return Number.isNaN(d.getTime()) ? null : d;
}
