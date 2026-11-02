import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter, Route, Routes } from 'react-router';
import { AuthProvider } from '../../app/auth';
import type { PortalUser } from '../../app/types';
import { portalApi } from '../../app/api';
import { ToastProvider } from '../../app/toast';
import { AdminRecords } from './AdminRecords';

type Access = Awaited<ReturnType<typeof portalApi.adminRecordAccess>>;
type Settings = Awaited<ReturnType<typeof portalApi.adminRecordSettings>>;
type RecordPage = Awaited<ReturnType<typeof portalApi.adminRecords>>;
type RecordItem = RecordPage['items'][number];

const fullRecordConsent: Access['consents'][number] = { owner_id: 'uowner01', purpose: 'full_record', enabled: false, version: 0, updated_at: '' };
const sampleConsent: Access['consents'][number] = { owner_id: 'uowner01', purpose: 'training_contribution', enabled: false, version: 0, updated_at: '' };
const defaultAccess: Access = { capabilities: [], consents: [fullRecordConsent, sampleConsent] };
const defaultSettings: Settings = {
  owner_id: 'uowner01',
  consents: defaultAccess.consents,
  quota: { owner_id: 'uowner01', retention_days: 30, limit_bytes: 1_073_741_824, used_bytes: 0, reserved_bytes: 0, version: 1, updated_at: '2026-11-02T00:00:00Z' },
  grants: [],
};
const oneRecord: RecordItem = {
  id: 'vrecord1', owner_id: 'uowner01', purpose: 'full_record', kind: 'turn', consent_version: 1,
  created_at: '2026-11-01T00:00:00Z', expires_at: '2026-12-01T00:00:00Z', payload_bytes: 32,
  attachment_bytes: 8, attachment_count: 1, state: 'active',
};

function renderPage(role: PortalUser['role']) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.spyOn(portalApi, 'me').mockResolvedValue({ id: 'uadmin01', name: 'admin', role, must_change_password: false } as never);
  return render(<QueryClientProvider client={client}><AuthProvider><ToastProvider><MemoryRouter initialEntries={['/admin/users/uowner01/records']}><Routes>
    <Route path="/admin/users/:id/records" element={<AdminRecords />} />
  </Routes></MemoryRouter></ToastProvider></AuthProvider></QueryClientProvider>);
}

describe('admin records', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('shows default-off consent and skips record reads', async () => {
    const records = vi.spyOn(portalApi, 'adminRecords').mockResolvedValue({ items: [], next_before: '' });
    vi.spyOn(portalApi, 'adminRecordAccess').mockResolvedValue(defaultAccess);
    vi.spyOn(portalApi, 'adminRecordSettings').mockResolvedValue(defaultSettings);
    renderPage('super_admin');

    expect(await screen.findByRole('heading', { name: '记录与样本' })).toBeInTheDocument();
    expect(await screen.findByText('用户同意状态')).toBeInTheDocument();
    expect(screen.getAllByText('默认关闭')).toHaveLength(2);
    expect(await screen.findByText('当前管理员没有查看记录的授权。')).toBeInTheDocument();
    expect(screen.getByText('记录额度')).toBeInTheDocument();
    expect(records).not.toHaveBeenCalled();
    expect(screen.queryByRole('button', { name: /开启同意|设置同意/ })).not.toBeInTheDocument();
  });

  it('does not request or expose records without view capability', async () => {
    const records = vi.spyOn(portalApi, 'adminRecords').mockResolvedValue({ items: [oneRecord], next_before: '' });
    vi.spyOn(portalApi, 'adminRecordAccess').mockResolvedValue({
      capabilities: [],
      consents: [{ ...fullRecordConsent, enabled: true, version: 1, updated_at: '2026-11-01T00:00:00Z' }, sampleConsent],
    });
    const settings = vi.spyOn(portalApi, 'adminRecordSettings').mockResolvedValue(defaultSettings);
    renderPage('admin');

    expect(await screen.findByText('当前管理员没有查看记录的授权。')).toBeInTheDocument();
    expect(records).not.toHaveBeenCalled();
    expect(settings).not.toHaveBeenCalled();
    expect(screen.queryByText(oneRecord.id)).not.toBeInTheDocument();
  });

  it('allows view only when granted and keeps attachment, export, and delete controls separate', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'adminRecordAccess').mockResolvedValue({
      capabilities: ['view'],
      consents: [{ ...fullRecordConsent, enabled: true, version: 1, updated_at: '2026-11-01T00:00:00Z' }, sampleConsent],
    });
    vi.spyOn(portalApi, 'adminRecords').mockResolvedValue({ items: [oneRecord], next_before: '' });
    vi.spyOn(portalApi, 'adminRecord').mockResolvedValue({ metadata: oneRecord, payload: 'synthetic record body' });
    const attachments = vi.spyOn(portalApi, 'adminRecordAttachments').mockResolvedValue({ items: [] });
    const exportRecord = vi.spyOn(portalApi, 'adminRecordExport').mockResolvedValue(new Blob(['{}'], { type: 'application/json' }));
    const deleteRecord = vi.spyOn(portalApi, 'deleteAdminRecord').mockResolvedValue({ state: 'pending' } as never);
    renderPage('admin');

    await user.click(await screen.findByRole('button', { name: `查看 ${oneRecord.id}` }));
    expect(await screen.findByText('synthetic record body')).toBeInTheDocument();
    expect(screen.getByText('当前管理员没有附件权限。')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '导出 JSON' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '删除记录' })).not.toBeInTheDocument();
    expect(attachments).not.toHaveBeenCalled();
    expect(exportRecord).not.toHaveBeenCalled();
    expect(deleteRecord).not.toHaveBeenCalled();
  });

  it('requires a second confirmation before deleting an authorized record', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'adminRecordAccess').mockResolvedValue({
      capabilities: ['view', 'delete'],
      consents: [{ ...fullRecordConsent, enabled: true, version: 1, updated_at: '2026-11-01T00:00:00Z' }, sampleConsent],
    });
    vi.spyOn(portalApi, 'adminRecords').mockResolvedValue({ items: [oneRecord], next_before: '' });
    vi.spyOn(portalApi, 'adminRecord').mockResolvedValue({ metadata: oneRecord, payload: 'synthetic record body' });
    const remove = vi.spyOn(portalApi, 'deleteAdminRecord').mockResolvedValue({ state: 'pending' } as never);
    renderPage('admin');

    await user.click(await screen.findByRole('button', { name: `查看 ${oneRecord.id}` }));
    await screen.findByText('synthetic record body');
    await user.click(screen.getByRole('button', { name: '删除记录' }));
    expect(remove).not.toHaveBeenCalled();
    expect(screen.getByRole('group', { name: '确认删除记录' })).toHaveTextContent('不能撤销');
    await user.click(screen.getByRole('button', { name: '确认删除记录' }));
    await waitFor(() => expect(remove).toHaveBeenCalledWith('uowner01', oneRecord.id));
  });
});
