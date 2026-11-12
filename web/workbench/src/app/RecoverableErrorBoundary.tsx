import { Component, type ReactNode } from 'react';
import { returnToPreviousPage } from './navigation';

export class RecoverableErrorBoundary extends Component<{ children: ReactNode }, { failed: boolean }> {
  state = { failed: false };

  static getDerivedStateFromError() { return { failed: true }; }

  render() {
    if (!this.state.failed) return this.props.children;
    return <main className="workbench-app workbench-error-app">
      <section className="page-message page-error" role="alert">
        <span className="page-error-mark" aria-hidden="true">!</span>
        <h1>页面暂时无法显示</h1>
        <p>服务或页面内容暂时不可用。可以返回上一页，或重试当前页面。</p>
        <div className="page-error-actions">
          <button className="button button-secondary" type="button" onClick={returnToPreviousPage}>返回上一页</button>
          <button className="button" type="button" onClick={() => this.setState({ failed: false })}>重试</button>
        </div>
      </section>
    </main>;
  }
}
