import { Link, useNavigate } from 'react-router';

export function RecoverableError({
  title,
  message,
  homePath = '/assistant',
  homeLabel = '返回助手',
  onRetry,
}: {
  title: string;
  message: string;
  homePath?: string;
  homeLabel?: string;
  onRetry?: () => void;
}) {
  const navigate = useNavigate();
  const goBack = () => {
    const index = (window.history.state as { idx?: unknown } | null)?.idx;
    if (typeof index === 'number' && Number.isInteger(index) && index > 0) navigate(-1);
    else navigate(homePath, { replace: true });
  };
  return <main className="loading-page">
    <section className="empty-card not-found recoverable-error" role="alert">
      <span className="recoverable-error-mark" aria-hidden="true">!</span>
      <h1>{title}</h1>
      <p>{message}</p>
      <div className="recoverable-error-actions">
        <button type="button" className="button button-secondary" onClick={goBack}>返回上一页</button>
        {onRetry && <button type="button" className="button button-primary" onClick={onRetry}>重试</button>}
        <Link className="button button-quiet" to={homePath}>{homeLabel}</Link>
      </div>
    </section>
  </main>;
}
