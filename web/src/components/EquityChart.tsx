import { Area, AreaChart, ReferenceDot, ReferenceLine, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts';
import { fmtMoneyPlain } from '../lib/format';
import type { EquityView } from '../lib/transform';

const UP = '#30D158';
const DOWN = '#FF453A';
const FAINT = '#636366';

export function EquityChart({ view }: { view: EquityView }) {
  if (!view.hasData) {
    return <div className="flex h-56 items-center justify-center text-[15px] text-faint">No equity history yet</div>;
  }

  // Apple Stocks semantics: the whole curve is tinted by the period's result.
  const color = view.lastIsUp ? UP : DOWN;
  const gradientId = view.lastIsUp ? 'eq-fill-up' : 'eq-fill-down';
  const last = view.points[view.points.length - 1];

  return (
    <div className="h-64 w-full">
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart data={view.points} margin={{ top: 8, right: 8, left: 8, bottom: 0 }}>
          <defs>
            <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={color} stopOpacity={0.3} />
              <stop offset="100%" stopColor={color} stopOpacity={0} />
            </linearGradient>
          </defs>
          <XAxis
            dataKey="label"
            tick={{ fill: FAINT, fontSize: 11 }}
            axisLine={false}
            tickLine={false}
            minTickGap={64}
            dy={6}
          />
          {/* hidden axis still drives the auto domain */}
          <YAxis hide domain={['auto', 'auto']} />
          {view.inceptionEquity !== null && (
            <ReferenceLine y={view.inceptionEquity} stroke={FAINT} strokeDasharray="3 4" strokeWidth={1} />
          )}
          <Tooltip
            contentStyle={{
              background: '#1C1C1E',
              border: '1px solid #38383A',
              borderRadius: 10,
              fontSize: 12,
            }}
            labelStyle={{ color: FAINT }}
            itemStyle={{ color: '#F5F5F7' }}
            formatter={(v: number) => [fmtMoneyPlain(v), 'equity']}
          />
          <Area
            type="monotone"
            dataKey="equity"
            stroke={color}
            strokeWidth={2}
            fill={`url(#${gradientId})`}
            dot={false}
            isAnimationActive
          />
          {last && <ReferenceDot x={last.label} y={last.equity} r={4} fill={color} stroke="none" isFront />}
        </AreaChart>
      </ResponsiveContainer>
    </div>
  );
}
