import { useState } from 'react';
import { Modal } from './Modal';
import { useToast } from '../app/toast';

export function OnceSecretDialog({ title, secret, expiresAt, onClose }: { title: string; secret: string; expiresAt: string; onClose: () => void }) {
  const [copied, setCopied] = useState(false);
  const { notify } = useToast();
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(secret);
      setCopied(true);
      notify('已复制。请妥善保存，这段凭证关闭后无法再次查看。', 'success');
    } catch {
      notify('无法访问剪贴板，请手动选中并复制。', 'error');
    }
  };
  return <Modal title={title} onClose={onClose}>
    <div className="modal-body">
      <p className="notice notice-warning">此凭证仅显示这一次，请立即复制并妥善保存。</p>
      <label htmlFor="one-time-secret">凭证</label>
      <textarea id="one-time-secret" className="secret-field" rows={3} readOnly value={secret} onFocus={(event) => event.currentTarget.select()} />
      <p className="muted">有效期至：{expiresAt}</p>
    </div>
    <footer className="modal-actions">
      <button type="button" className="button button-secondary" onClick={onClose}>关闭并清除</button>
      <button type="button" className="button button-primary" onClick={() => void copy()}>{copied ? '已复制' : '复制凭证'}</button>
    </footer>
  </Modal>;
}
