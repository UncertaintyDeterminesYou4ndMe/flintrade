import { useEffect, useRef, useState } from 'react';

/**
 * Fade-up-once-on-scroll-into-view. Returns a ref to attach to the section
 * element plus a boolean that flips true the first time it enters the
 * viewport and never resets. CSS (.reveal / .reveal-in in index.css) does
 * the actual transform/opacity transition and already respects
 * prefers-reduced-motion, so this hook only tracks visibility.
 */
export function useReveal<T extends HTMLElement>() {
  const ref = useRef<T | null>(null);
  const [revealed, setRevealed] = useState(false);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (typeof IntersectionObserver === 'undefined') {
      setRevealed(true);
      return;
    }
    const io = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (entry.isIntersecting) {
            setRevealed(true);
            io.disconnect();
          }
        }
      },
      { threshold: 0.08 },
    );
    io.observe(el);
    return () => io.disconnect();
  }, []);

  return { ref, revealed };
}
