import { useState } from 'react';
import type { FormEvent } from 'react';
import { Link, Navigate, useNavigate } from 'react-router';
import { errorDetails, portalApi } from '../app/api';
import { useAuth } from '../app/auth';

export function Register() {
  const { user, loading } = useAuth();
  const navigate = useNavigate();
  const [name, setName] = useState('');
  const [password, setPassword] = useState('');
  const [confirmation, setConfirmation] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  if (loading) return <main className="loading-page">正在检查登录状态…</main>;
  if (user) return <Navigate to="/" replace />;

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (password !== confirmation) { setError('两次输入的密码不一致。'); return; }
    setBusy(true); setError('');
    try {
      await portalApi.register(name.trim(), password);
      navigate('/login', { replace: true, state: { registered: true } });
    } catch (reason) {
      const details = errorDetails(reason);
      setError(details.code === 'conflict' ? '用户名已被使用，请更换用户名。' : details.message);
    } finally { setBusy(false); }
  };

  return <main className="login-page">
    <section className="login-card" aria-labelledby="register-heading">
      <div className="login-brand"><span className="brand-mark">M</span><span>Morde 云端电脑</span></div>
      <h1 id="register-heading">注册账号</h1>
      <p className="muted">提交注册申请后，管理员审核通过即可登录用户 Web。</p>
      <form onSubmit={(event) => void submit(event)}>
        <label htmlFor="register-name">用户名</label>
        <input id="register-name" name="name" autoComplete="username" required minLength={3} maxLength={64} value={name} onChange={(event) => setName(event.target.value)} />
        <label htmlFor="register-password">密码</label>
        <input id="register-password" name="password" type="password" autoComplete="new-password" required minLength={8} maxLength={72} value={password} onChange={(event) => setPassword(event.target.value)} />
        <label htmlFor="register-confirmation">确认密码</label>
        <input id="register-confirmation" name="confirmation" type="password" autoComplete="new-password" required value={confirmation} onChange={(event) => setConfirmation(event.target.value)} />
        {error && <p className="form-error" role="alert">{error}</p>}
        <button className="button button-primary button-wide" type="submit" disabled={busy}>{busy ? '正在注册…' : '注册'}</button>
      </form>
      <p className="login-register-link">已有账号？<Link to="/login">返回登录</Link></p>
    </section>
  </main>;
}
