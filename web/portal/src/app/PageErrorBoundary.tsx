import { Component, type ReactNode } from 'react';
import { useLocation } from 'react-router';
import { RecoverableError } from '../components/RecoverableError';

type BoundaryProps = { children: ReactNode; homePath: string; homeLabel: string };
type BoundaryState = { failed: boolean };

class PageErrorBoundary extends Component<BoundaryProps, BoundaryState> {
  state: BoundaryState = { failed: false };

  static getDerivedStateFromError(): BoundaryState { return { failed: true }; }

  render() {
    if (!this.state.failed) return this.props.children;
    return <RecoverableError
      title="页面暂时无法显示"
      message="服务或页面内容暂时不可用。可以返回上一页，或重试当前页面。"
      homePath={this.props.homePath}
      homeLabel={this.props.homeLabel}
      onRetry={() => this.setState({ failed: false })}
    />;
  }
}

export function RecoverablePageBoundary({ children, homePath, homeLabel }: BoundaryProps) {
  const location = useLocation();
  return <PageErrorBoundary key={location.key} homePath={homePath} homeLabel={homeLabel}>{children}</PageErrorBoundary>;
}
