import type { ReactNode } from 'react';
import { Modal } from './Modal';

export function ConfirmDialog({ title, description, confirmLabel = '确认', danger = false, busy = false, onClose, onConfirm, children }: {
  title: string; description: string; confirmLabel?: string; danger?: boolean; busy?: boolean; onClose: () => void; onConfirm: () => void; children?: ReactNode;
}) {
  return <Modal title={title} onClose={onClose}>
    <div className="modal-body"><p>{description}</p>{children}</div>
    <footer className="modal-actions">
      <button type="button" className="button button-secondary" onClick={onClose} disabled={busy}>取消</button>
      <button type="button" className={danger ? 'button button-danger' : 'button button-primary'} disabled={busy} onClick={onConfirm}>
        {busy ? '处理中…' : confirmLabel}
      </button>
    </footer>
  </Modal>;
}
