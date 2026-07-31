import { HaltBand, OfflineBand, StickyBar } from './components/StickyBar';
import { Masthead } from './components/Masthead';
import { Section } from './components/Section';
import { FrontPage } from './sections/FrontPage';
import { Ledger } from './sections/Ledger';
import { Blotter } from './sections/Blotter';
import { DreamJournal } from './sections/DreamJournal';
import { Colophon } from './sections/Colophon';
import {
  useRisk,
  useConfig,
  usePositions,
  useTrades,
  useIntents,
  useProcesses,
  useEquityCurve,
  useReviews,
  useBrain,
  anyError,
} from './lib/hooks';

function App() {
  const risk = useRisk();
  const config = useConfig();
  const positions = usePositions();
  const trades = useTrades();
  const intents = useIntents();
  const processes = useProcesses();
  const equityCurve = useEquityCurve();
  const reviews = useReviews();
  const brain = useBrain();

  const offline = anyError(risk, config, positions, trades, intents, processes, equityCurve, reviews, brain);
  const halted = risk.data?.halt === 1;

  return (
    <div className="min-h-screen bg-surface text-fg">
      <StickyBar session={config.data?.session ?? '—'} healthy={!offline} />
      {halted && <HaltBand reason={risk.data?.halt_reason} />}
      {offline && <OfflineBand />}

      <Masthead />

      <main>
        <Section id="front-page" label="Front Page">
          <FrontPage />
        </Section>
        <Section id="ledger" label="The Ledger">
          <Ledger />
        </Section>
        <Section id="blotter" label="Order Blotter">
          <Blotter />
        </Section>
        <Section id="dream-journal" label="Dream Journal">
          <DreamJournal />
        </Section>
        <Section id="colophon" label="Colophon">
          <Colophon />
        </Section>
      </main>
    </div>
  );
}

export default App;
