const TODAY_LABEL = new Date().toLocaleDateString('en-US', {
  month: 'long',
  day: 'numeric',
  year: 'numeric',
});

export function Masthead() {
  return (
    <header className="mx-auto max-w-6xl px-6 pb-2 pt-10">
      <h1 className="text-[34px] font-bold leading-tight tracking-tight text-fg">Flintrade Ledger</h1>
      <p className="mt-1 text-[13px] text-muted">{TODAY_LABEL} · Paper trading · est. 2026-06-17</p>
    </header>
  );
}
