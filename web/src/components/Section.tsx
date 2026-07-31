import type { ReactNode } from 'react';
import { useReveal } from '../hooks/useReveal';
import { ErrorBoundary } from './ErrorBoundary';

interface SectionProps {
  id: string;
  label: string;
  children: ReactNode;
  className?: string;
}

/** One top-level page section: fade-up-once on scroll-into-view, wrapped in
 * its own error boundary so a crash here can't black-screen the rest of the
 * front page. */
export function Section({ id, label, children, className }: SectionProps) {
  const { ref, revealed } = useReveal<HTMLElement>();
  return (
    <section
      id={id}
      ref={ref}
      className={`reveal scroll-mt-16 ${revealed ? 'reveal-in' : ''} ${className ?? ''}`}
    >
      <ErrorBoundary label={label}>{children}</ErrorBoundary>
    </section>
  );
}
