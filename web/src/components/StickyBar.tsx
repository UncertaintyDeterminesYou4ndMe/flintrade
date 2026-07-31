import { useActiveSection } from '../hooks/useActiveSection';

const NAV = [
  { id: 'front-page', label: 'Front Page' },
  { id: 'ledger', label: 'Ledger' },
  { id: 'blotter', label: 'Blotter' },
  { id: 'dream-journal', label: 'Dream Journal' },
  { id: 'colophon', label: 'Colophon' },
];

const NAV_IDS = NAV.map((s) => s.id);

interface StickyBarProps {
  session: string;
  healthy: boolean;
}

export function StickyBar({ session, healthy }: StickyBarProps) {
  const activeId = useActiveSection(NAV_IDS);
  return (
    <nav className="sticky top-0 z-50 border-b border-line bg-surface/80 backdrop-blur-xl">
      <div className="mx-auto flex max-w-6xl flex-wrap items-center justify-between gap-x-6 gap-y-1 px-6 py-3">
        <div className="flex flex-wrap items-center gap-x-5 text-[13px]">
          {NAV.map((s) => (
            <a
              key={s.id}
              href={`#${s.id}`}
              className={`transition-colors hover:text-fg ${
                activeId === s.id ? 'font-semibold text-fg' : 'text-muted'
              }`}
            >
              {s.label}
            </a>
          ))}
        </div>
        <div className="flex items-center gap-1.5 rounded-full bg-card px-2.5 py-1 text-[12px] text-muted">
          <span>{session}</span>
          <span
            className={`inline-block h-1.5 w-1.5 shrink-0 rounded-full dot-breathe ${healthy ? 'bg-up' : 'bg-down'}`}
          />
        </div>
      </div>
    </nav>
  );
}

export function HaltBand({ reason }: { reason?: string | null }) {
  return (
    <div className="border-b border-down bg-down px-6 py-2">
      <p className="mx-auto max-w-6xl text-[12px] font-semibold tracking-wide text-white">
        TRADING HALTED{reason ? ` — ${reason}` : ''}
      </p>
    </div>
  );
}

export function OfflineBand() {
  return (
    <div className="border-b border-line bg-card px-6 py-1.5">
      <p className="mx-auto max-w-6xl text-[12px] text-down">
        backend unreachable — some sections may be showing stale or missing data
      </p>
    </div>
  );
}
