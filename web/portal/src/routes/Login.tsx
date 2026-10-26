import { useState } from 'react';
import type { FormEvent } from 'react';
import { Link, Navigate, useLocation, useNavigate } from 'react-router';
import { errorDetails } from '../app/api';
import { useAuth } from '../app/auth';

export function Login({ defaultTo = '/assistant', allowRegister = true }: { defaultTo?: string; allowRegister?: boolean }) {
  const { user, loading, signIn } = useAuth();
  const location = useLocation();
  const navigate = useNavigate();
  const [name, setName] = useState('');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const stateFrom = (location.state as { from?: string } | null)?.from;
  const queryFrom = new URLSearchParams(location.search).get('next');
  const registered = (location.state as { registered?: boolean } | null)?.registered === true;
  const requested = stateFrom || queryFrom || defaultTo;
  const from = requested.startsWith('/') && !requested.startsWith('//') ? requested : defaultTo;
  if (loading) return <main className="loading-page">正在检查登录状态…</main>;
  if (user) return <Navigate to={from} replace />;

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setBusy(true); setError('');
    try { await signIn(name.trim(), password); navigate(from, { replace: true }); }
    catch (reason) {
      const details = errorDetails(reason);
      if (details.code === 'rate_limited') setError(`登录尝试过于频繁。${details.retryAfter ? `请在 ${details.retryAfter} 秒后重试。` : '请稍后重试。'}`);
      else if (details.code === 'unauthenticated') setError('用户名或密码错误，请重试。');
      else if (details.code === 'account_pending') setError('账号正在等待管理员审核，通过后才能登录。');
      else if (details.code === 'account_rejected') setError('账号审核未通过，请联系管理员。');
      else if (details.code === 'account_disabled') setError('账号已被禁用，请联系管理员。');
      else setError(details.message);
    } finally { setBusy(false); }
  };

  return <main className="login-page">
    <section className="login-card" aria-labelledby="login-heading">
      <div className="login-brand"><span className="brand-mark">M</span><span>Morde 云端电脑</span></div>
      <h1 id="login-heading">登录</h1>
      <p className="muted">登录后交办云端任务，管理你的电脑与文件。</p>
      <Link className="login-download-link" to="/download">下载 Morde Desktop</Link>
      {registered && <p className="notice notice-success" role="status">申请已提交，等待管理员审核通过后才能登录。</p>}
      <form onSubmit={(event) => void submit(event)}>
        <label htmlFor="login-name">用户名</label>
        <input id="login-name" name="name" autoComplete="username" required value={name} onChange={(event) => setName(event.target.value)} />
        <label htmlFor="login-password">密码</label>
        <input id="login-password" name="password" type="password" autoComplete="current-password" required value={password} onChange={(event) => setPassword(event.target.value)} />
        {error && <div className="form-error" role="alert">{error}</div>}
        <button className="button button-primary button-wide" type="submit" disabled={busy}>{busy ? '正在登录…' : '登录'}</button>
      </form>
      {allowRegister && <p className="login-register-link">还没有账号？<Link to="/register">注册账号</Link></p>}
    </section>
  </main>;
}
