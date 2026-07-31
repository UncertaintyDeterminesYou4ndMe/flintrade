// Data-level smoke test for lib/transform.ts.
//
// Run with: node --experimental-strip-types scripts/smoke.mts
// (also wired as `npm run smoke`)
//
// Fetches the live backend endpoints and runs every transform function
// against the real response shapes. This is the gate that catches shape
// crashes at the data level — e.g. the confidence_buckets object (keyed
// "<60"/"60-70"/">70", each value possibly {win_rate: null}) that crashed
// an earlier build of this dashboard.
//
// No assertions on business values — this only proves each transform can
// run end-to-end against whatever the backend actually returns right now
// without throwing, and sanity-checks the output shape.

import {
  toEquityView,
  toLedgerRows,
  toBlotterRows,
  toDreamView,
  toColophon,
  toDeskView,
  toPositionRows,
  toFrontStats,
} from '../src/lib/transform.ts';
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
} from '../src/lib/types.ts';

const BASE = process.env.FLINTRADE_API_BASE ?? 'http://localhost:8383';

async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(`${BASE}${path}`);
  if (!res.ok) throw new Error(`${path} -> HTTP ${res.status}`);
  return (await res.json()) as T;
}

interface Check {
  name: string;
  fn: () => void | Promise<void>;
}

let pass = 0;
let fail = 0;

async function run(checks: Check[]): Promise<void> {
  for (const c of checks) {
    try {
      await c.fn();
      pass += 1;
      console.log(`PASS  ${c.name}`);
    } catch (err) {
      fail += 1;
      const msg = err instanceof Error ? err.stack ?? err.message : String(err);
      console.log(`FAIL  ${c.name}\n      ${msg.split('\n').join('\n      ')}`);
    }
  }
}

function assert(cond: unknown, msg: string): asserts cond {
  if (!cond) throw new Error(`assertion failed: ${msg}`);
}

async function main(): Promise<void> {
  console.log(`smoke: fetching live endpoints from ${BASE}`);

  const endpoints = [
    'risk',
    'positions',
    'trades',
    'intents',
    'processes',
    'equity-curve',
    'reviews',
    'brain',
    'config',
  ] as const;

  const fetched: Record<string, unknown> = {};
  await run(
    endpoints.map((ep) => ({
      name: `fetch /api/${ep}`,
      fn: async () => {
        fetched[ep] = await getJSON(`/api/${ep}`);
      },
    })),
  );

  if (fail > 0) {
    console.log(`\n${pass} passed, ${fail} failed (aborting before transform checks — endpoints unreachable)`);
    process.exit(1);
  }

  const risk = fetched.risk as RiskState;
  const positions = fetched.positions as Position[];
  const trades = fetched.trades as Trade[];
  const intents = fetched.intents as Intent[];
  const processes = fetched.processes as ProcessEntry[];
  const equityCurve = fetched['equity-curve'] as EquityCurve;
  const reviews = fetched.reviews as ReviewsResponse;
  const brain = fetched.brain as BrainResponse;
  const config = fetched.config as Config;

  await run([
    {
      name: 'toEquityView',
      fn: () => {
        const v = toEquityView(equityCurve, risk);
        assert(Array.isArray(v.points), 'points is an array');
        assert(typeof v.summaryLine === 'string', 'summaryLine is a string');
      },
    },
    {
      name: 'toEquityView (null/missing inputs)',
      fn: () => {
        const v = toEquityView(null, undefined);
        assert(v.points.length === 0, 'points empty on null input');
        assert(v.returnPct === null, 'returnPct null on null input');
      },
    },
    {
      name: 'toLedgerRows',
      fn: () => {
        const rows = toLedgerRows(trades, reviews);
        assert(Array.isArray(rows), 'rows is an array');
        assert(rows.length === (trades?.length ?? 0), 'row count matches trade count');
        for (const r of rows) {
          assert(typeof r.tsLabel === 'string', 'tsLabel is a string');
          assert(typeof r.pnlLabel === 'string', 'pnlLabel is a string');
        }
      },
    },
    {
      name: 'toLedgerRows (null/missing inputs)',
      fn: () => {
        const rows = toLedgerRows(null, null);
        assert(rows.length === 0, 'empty on null input');
      },
    },
    {
      name: 'toBlotterRows',
      fn: () => {
        const rows = toBlotterRows(intents);
        assert(Array.isArray(rows), 'rows is an array');
        assert(rows.length === (intents?.length ?? 0), 'row count matches intent count');
      },
    },
    {
      name: 'toBlotterRows (null/missing inputs)',
      fn: () => {
        const rows = toBlotterRows(undefined);
        assert(rows.length === 0, 'empty on undefined input');
      },
    },
    {
      name: 'toDreamView — confidence_buckets shape (the historical crash source)',
      fn: () => {
        const v = toDreamView(brain, reviews);
        assert(v.calibration.length === 3, 'exactly 3 buckets: <60, 60-70, >70');
        assert(v.calibration.map((c) => c.bucket).join(',') === '<60,60-70,>70', 'bucket order preserved');
        for (const row of v.calibration) {
          if (!row.hasData) assert(row.winRatePct === null, `${row.bucket}: null win_rate handled as no-data`);
        }
      },
    },
    {
      name: 'toDreamView (null/missing inputs)',
      fn: () => {
        const v = toDreamView(null, null);
        assert(v.calibration.length === 3, 'still 3 buckets with no data at all');
        assert(v.lessons.length === 0, 'no lessons on null brain');
        assert(v.plans.length === 0, 'no plans on null brain');
      },
    },
    {
      name: 'toColophon',
      fn: () => {
        const v = toColophon(config);
        assert(Array.isArray(v.houseRules), 'houseRules is an array');
        assert(Array.isArray(v.universe), 'universe is an array');
        assert(Array.isArray(v.cadence), 'cadence is an array');
      },
    },
    {
      name: 'toColophon (null/missing inputs)',
      fn: () => {
        const v = toColophon(undefined);
        assert(v.houseRules.length === 0, 'empty house rules on null config');
        assert(v.universe.length === 0, 'empty universe on null config');
      },
    },
    {
      name: 'toDeskView',
      fn: () => {
        const v = toDeskView(risk, processes, brain?.recall?.time_anchor, config?.session);
        assert(Array.isArray(v.wire), 'wire is an array');
        assert(v.wire.length === (processes?.length ?? 0), 'wire row count matches process count');
      },
    },
    {
      name: 'toDeskView (null/missing inputs)',
      fn: () => {
        const v = toDeskView(null, undefined, undefined, undefined);
        assert(v.wire.length === 0, 'empty wire on undefined processes');
        assert(v.session === '—', 'session falls back to em-dash');
      },
    },
    {
      name: 'toPositionRows',
      fn: () => {
        const rows = toPositionRows(positions);
        assert(rows.length === (positions?.length ?? 0), 'row count matches position count');
      },
    },
    {
      name: 'toPositionRows (null/missing inputs)',
      fn: () => {
        const rows = toPositionRows(null);
        assert(rows.length === 0, 'empty on null input');
      },
    },
    {
      name: 'toFrontStats',
      fn: () => {
        const v = toFrontStats(risk, equityCurve, brain);
        assert(typeof v.dayPnlLabel === 'string', 'dayPnlLabel is a string');
        assert(['up', 'down', 'flat'].includes(v.dayTone), 'dayTone is a valid tone');
        assert(['up', 'down', 'flat'].includes(v.returnTone), 'returnTone is a valid tone');
        assert(typeof v.winRateLabel === 'string', 'winRateLabel is a string');
      },
    },
    {
      name: 'toFrontStats (null/missing inputs)',
      fn: () => {
        const v = toFrontStats(null, undefined, undefined);
        assert(v.dayTone === 'flat', 'flat tone on null day pnl');
        assert(v.winRateLabel === '—', 'win rate falls back to em-dash');
      },
    },
  ]);

  console.log(`\n${pass} passed, ${fail} failed`);
  if (fail > 0) process.exit(1);
}

main().catch((err) => {
  console.error('smoke: unexpected fatal error');
  console.error(err);
  process.exit(1);
});
