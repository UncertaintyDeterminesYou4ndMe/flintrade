import { Component, type ReactNode } from 'react';

interface Props {
  children: ReactNode;
  label: string;
}

interface State {
  error: Error | null;
}

/**
 * Wraps a single page section. A crash inside it renders a one-line red
 * mono notice instead of black-screening the whole page — sections are
 * independent enough (each backed by its own query) that one bad shape
 * shouldn't take down the rest of the front page.
 */
export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: { componentStack?: string | null }) {
    console.error(`[${this.props.label}] section crashed`, error, info.componentStack);
  }

  render() {
    if (this.state.error) {
      return (
        <p className="text-xs text-down">this section failed to render: {this.state.error.message}</p>
      );
    }
    return this.props.children;
  }
}
