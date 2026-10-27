import { useEffect, useRef, useState } from 'react';
import { Link, useLocation, useParams } from 'react-router';
import { errorDetails, portalApi } from '../app/api';

type EntryTarget = 'desktop' | 'terminal' | null;

function parseEntryTarget(search: string): EntryTarget {
  const params = new URLSearchParams(search);
  const targets = params.getAll('target');
  if (params.size !== targets.length || targets.length > 1 || (targets.length === 1 && targets[0] !== 'desktop' && targets[0] !== 'terminal')) {
    throw new Error('云端电脑入口参数无效。');
  }
  return targets.length === 0 ? null : targets[0] as 'desktop' | 'terminal';
}

export function resolveHostEntryURL(value: string, hostURL: string, portalHostname = window.location.hostname): string {
  let host: URL;
  let target: URL;
  try {
    host = new URL(hostURL);
    target = new URL(value, host);
  } catch {
    throw new Error('服务器返回了无效的云端电脑入口。');
  }
  const next = target.searchParams.get('next');
  const localHTTP = (portalHostname === 'portal.localhost' || portalHostname === 'localhost') && host.protocol === 'http:' && /^h[a-z0-9]{7}\.hosts\.localhost$/.test(host.hostname) && !host.port;
  if ((!localHTTP && host.protocol !== 'https:') || host.username || host.password || host.hash || host.search || host.pathname !== '/'
    || target.origin !== host.origin || target.pathname !== '/__ch/auth'
    || target.searchParams.getAll('t').length !== 1 || !target.searchParams.get('t')
    || target.searchParams.getAll('next').length > 1
    || [...target.searchParams.keys()].some((key) => key !== 't' && key !== 'next')
    || (next !== null && (!next.startsWith('/') || next.startsWith('//') || next.includes('\\')))
    || target.username || target.password || target.hash) {
    throw new Error('服务器返回了无效的云端电脑入口。');
  }
  return target.href;
}

export async function fetchEntryURL(id: string, target: EntryTarget): Promise<string> {
  if (target) {
    const { items } = await portalApi.sharedHosts();
    const host = items.find((item) => item.id === id);
    const permitted = target === 'terminal'
      ? host?.permissions.includes('terminal:control')
      : host?.permissions.includes('desktop:view') || host?.permissions.includes('desktop:control');
    if (!host || !permitted || host.state !== 'running') throw new Error('这台共享电脑已不可访问。');
    const ticket = await portalApi.issueObservationTicket(id, target);
    return resolveHostEntryURL(ticket.url, host.url);
  }
  const host = await portalApi.host(id);
  const ticket = await portalApi.enterHost(id);
  return resolveHostEntryURL(ticket.url, host.url);
}

export function HostEntry() {
  const { id = '' } = useParams();
  const { search } = useLocation();
  const [error, setError] = useState('');
  const activeAttempt = useRef<{ key: string; active: boolean } | null>(null);

  useEffect(() => {
    const key = `${id}${search}`;
    let attempt = activeAttempt.current;
    if (!attempt || attempt.key !== key) {
      const nextAttempt = { key, active: true };
      attempt = nextAttempt;
      activeAttempt.current = nextAttempt;
      setError('');
      void Promise.resolve()
        .then(() => fetchEntryURL(id, parseEntryTarget(search)))
        .then((destination) => {
          if (activeAttempt.current === nextAttempt && nextAttempt.active) window.location.replace(destination);
        })
        .catch((reason: unknown) => {
          if (activeAttempt.current === nextAttempt && nextAttempt.active) setError(errorDetails(reason).message);
        });
    } else {
      attempt.active = true;
    }
    return () => {
      if (attempt && activeAttempt.current === attempt) attempt.active = false;
    };
  }, [id, search]);

  return <main className="loading-page" aria-live="polite">
    {error
      ? <section className="empty-card"><h1>无法进入云端电脑</h1><p role="alert">{error}</p><div className="inline-actions"><a className="button button-primary" href={`/host-entry/${encodeURIComponent(id)}${search}`}>重试</a><Link className="button button-secondary" to="/hosts">返回云端电脑</Link></div></section>
      : <p>正在连接云端电脑…</p>}
  </main>;
}
