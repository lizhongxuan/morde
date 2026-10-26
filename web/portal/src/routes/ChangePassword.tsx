import { useState } from 'react';
import type { FormEvent } from 'react';
import { Link } from 'react-router';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../app/api';

export function ChangePassword() {
  const client = useQueryClient();
  const [currentPassword, setCurrentPassword] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const [confirmation, setConfirmation] = useState('');
  const [error, setError] = useState('');
  const [changed, setChanged] = useState(false);
  const change = useMutation({
    mutationFn: portalApi.changePassword,
    onSuccess: async () => {
      await client.cancelQueries({ queryKey: ['auth', 'me'] });
      client.setQueryData(['auth', 'me'], null);
      setChanged(true);
    },
    onError: (reason) => setError(errorDetails(reason).message),
  });

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setError('');
    if (newPassword !== confirmation) { setError('两次输入的新密码不一致。'); return; }
    change.mutate({ current_password: currentPassword, new_password: newPassword });
  };

  return <main className="login-page">
    <section className="login-card" aria-labelledby="change-password-heading">
      <div className="login-brand"><span className="brand-mark">M</span><span>Morde 云端电脑</span></div>
      <h1 id="change-password-heading">{changed ? '密码已修改' : '修改密码'}</h1>
      {changed ? <>
        <p className="notice notice-success" role="status">密码已更新，现有登录会话已退出。请使用新密码重新登录。</p>
        <Link className="button button-primary button-wide" to="/admin/login">前往管理后台登录</Link>
      </> : <>
        <p className="muted">请输入当前密码和新的登录密码。新密码长度为 8–72 个字符。</p>
        <form onSubmit={submit}>
          <label htmlFor="current-password">当前密码</label>
          <input id="current-password" name="current_password" type="password" autoComplete="current-password" required maxLength={72} value={currentPassword} onChange={(event) => setCurrentPassword(event.target.value)} />
          <label htmlFor="new-password">新密码</label>
          <input id="new-password" name="new_password" type="password" autoComplete="new-password" required minLength={8} maxLength={72} value={newPassword} onChange={(event) => setNewPassword(event.target.value)} />
          <label htmlFor="confirm-password">确认新密码</label>
          <input id="confirm-password" name="confirm_password" type="password" autoComplete="new-password" required minLength={8} maxLength={72} value={confirmation} onChange={(event) => setConfirmation(event.target.value)} />
          {error && <div className="form-error" role="alert">{error}</div>}
          <button className="button button-primary button-wide" type="submit" disabled={change.isPending}>{change.isPending ? '正在保存…' : '修改密码'}</button>
        </form>
      </>}
    </section>
  </main>;
}
