import { useState } from 'react';
import type { FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import { formatDate } from '../../app/format';
import { useToast } from '../../app/toast';
import { Modal } from '../../components/Modal';

type AdminModelConnection = Awaited<ReturnType<typeof portalApi.adminModelConnections>>['items'][number];
type ModelConnectionForm = { name: string; baseURL: string; modelName: string; apiKey: string };

const emptyForm: ModelConnectionForm = { name: '', baseURL: '', modelName: '', apiKey: '' };

export function AdminModels() {
  const client = useQueryClient();
  const { notify } = useToast();
  const query = useQuery({ queryKey: ['admin', 'model-connections'], queryFn: portalApi.adminModelConnections });
  const [formOpen, setFormOpen] = useState(false);
  const [formID, setFormID] = useState<string | null>(null);
  const [form, setForm] = useState<ModelConnectionForm>(emptyForm);
  const [deleteID, setDeleteID] = useState<string | null>(null);
  const [checked, setChecked] = useState<{ id: string; capabilities: AdminModelConnection['capabilities'] } | null>(null);

  const refreshConnections = async () => {
    await client.invalidateQueries({ queryKey: ['admin', 'model-connections'] });
    void client.invalidateQueries({ queryKey: ['model-connections'] });
  };
  const create = useMutation({
    mutationFn: (value: ModelConnectionForm) => portalApi.createModelConnection({
      name: value.name.trim(), base_url: value.baseURL.trim(), model_name: value.modelName.trim(), api_key: value.apiKey, shared: true,
    }),
    onSuccess: async () => {
      setFormOpen(false);
      setFormID(null);
      setForm(emptyForm);
      await refreshConnections();
      notify('平台模型连接已添加。', 'success');
    },
  });
  const update = useMutation({
    mutationFn: ({ id, value }: { id: string; value: ModelConnectionForm }) => portalApi.updateModelConnection(id, {
      name: value.name.trim(), base_url: value.baseURL.trim(), model_name: value.modelName.trim(),
      ...(value.apiKey ? { api_key: value.apiKey } : {}),
    }),
    onSuccess: async () => {
      setFormOpen(false);
      setFormID(null);
      setForm(emptyForm);
      await refreshConnections();
      notify('平台模型连接已保存。', 'success');
    },
  });
  const remove = useMutation({
    mutationFn: (id: string) => portalApi.deleteModelConnection(id),
    onSuccess: async () => {
      setDeleteID(null);
      await refreshConnections();
      notify('平台模型连接已删除。', 'success');
    },
  });
  const check = useMutation({
    mutationFn: (id: string) => portalApi.checkModelConnection(id),
    onSuccess: async (capabilities, id) => {
      setChecked({ id, capabilities });
      await refreshConnections();
      notify('平台模型连接检查完成。', 'success');
    },
  });

  function closeForm() {
    if (formMutation.isPending) return;
    setFormOpen(false);
    setFormID(null);
    setForm(emptyForm);
  }

  function openCreate() {
    setFormOpen(true);
    setFormID(null);
    setForm(emptyForm);
    create.reset();
    update.reset();
  }

  function openEdit(model: AdminModelConnection) {
    setFormOpen(true);
    setFormID(model.id);
    setForm({ name: model.name, baseURL: model.base_url, modelName: model.model_name, apiKey: '' });
    create.reset();
    update.reset();
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (formMutation.isPending) return;
    if (!form.name.trim() || !form.baseURL.trim() || !form.modelName.trim()) return;
    if (formID) update.mutate({ id: formID, value: form });
    else if (form.apiKey) create.mutate(form);
  }

  const formMutation = formID ? update : create;
  const models = query.data?.items.filter((model) => model.shared) ?? [];

  return <section className="admin-content">
    <div className="page-heading"><div><p className="eyebrow">模型运营</p><h1>平台模型</h1><p className="muted">管理共享模型连接与连通性。密钥保存后不会再次显示。</p></div>
      <button type="button" className="button button-primary" onClick={openCreate}>＋ 添加平台模型</button></div>

    {query.isPending ? <div className="loading-card" role="status">正在加载平台模型连接…</div>
      : query.isError ? <div className="error-card" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.refetch()}>重试</button></div>
        : !models.length ? <div className="empty-card compact"><h2>尚未配置平台模型</h2><p>填写真实的 HTTPS 服务地址、模型名和密钥；当前没有预置供应商目录。</p><button type="button" className="button button-secondary" onClick={openCreate}>添加平台模型</button></div>
          : <div className="table-wrap"><table><thead><tr><th>连接</th><th>服务地址</th><th>密钥</th><th>能力 / 检查状态</th><th>更新时间</th><th>操作</th></tr></thead><tbody>
            {models.map((model) => <tr key={model.id}>
              <td><strong>{model.name}</strong><small className="table-sub">{model.model_name}</small></td>
              <td className="mono">{model.base_url}</td>
              <td>{model.has_api_key ? '已配置' : '未配置'}</td>
              <td>{model.checked_at ? <><span>{capabilitySummary(model.capabilities)}</span><small className="table-sub">已检查 · {formatDate(model.checked_at)}</small></> : <span className="muted">尚未检查</span>}
                {checked?.id === model.id && <p className="notice notice-info model-check-result">本次检查：{capabilitySummary(checked.capabilities)}</p>}
                {check.isError && check.variables === model.id && <p className="form-error" role="alert">{errorDetails(check.error).message}</p>}
              </td>
              <td>{formatDate(model.updated_at)}</td>
              <td><div className="inline-actions">
                <button type="button" className="button button-secondary button-small" onClick={() => openEdit(model)}>编辑</button>
                <button type="button" className="button button-secondary button-small" disabled={check.isPending} onClick={() => check.mutate(model.id)}>{check.isPending && check.variables === model.id ? '检查中…' : '检查连接'}</button>
                <button type="button" className="button button-danger-quiet button-small" disabled={remove.isPending} onClick={() => { setDeleteID(model.id); remove.reset(); }}>删除</button>
              </div>
                {deleteID === model.id && <div className="notice notice-warning" role="group" aria-label={`确认删除 ${model.name}`}>
                  <p>删除后，使用此共享连接的新模型调用将不可用。</p>
                  {remove.isError && <p role="alert">{errorDetails(remove.error).message}</p>}
                  <div className="inline-actions"><button type="button" className="button button-secondary button-small" onClick={() => { setDeleteID(null); remove.reset(); }}>取消</button><button type="button" className="button button-danger-quiet button-small" disabled={remove.isPending} onClick={() => remove.mutate(model.id)}>{remove.isPending ? '正在删除…' : '确认删除'}</button></div>
                </div>}
              </td>
            </tr>)}
          </tbody></table></div>}

    {formOpen && <Modal title={formID === null ? '添加平台模型连接' : '编辑平台模型连接'} onClose={closeForm}><ModelConnectionFormView form={form} setForm={setForm} isEditing={formID !== null} onSubmit={submit} pending={formMutation.isPending} error={formMutation.error} onCancel={closeForm} /></Modal>}
  </section>;
}

function ModelConnectionFormView({ form, setForm, isEditing, onSubmit, pending, error, onCancel }: {
  form: ModelConnectionForm;
  setForm: (value: ModelConnectionForm) => void;
  isEditing: boolean;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
  pending: boolean;
  error: Error | null;
  onCancel: () => void;
}) {
  return <form className="connection-form" onSubmit={onSubmit}>
    <p className="muted">仅维护共享的平台连接。请使用真实的 HTTPS 服务地址和模型名。</p>
    <div className="connection-form-grid">
      <div><label htmlFor="admin-model-name">显示名称</label><input id="admin-model-name" value={form.name} maxLength={80} required onChange={(event) => setForm({ ...form, name: event.target.value })} /></div>
      <div><label htmlFor="admin-model-provider-name">模型名</label><input id="admin-model-provider-name" value={form.modelName} maxLength={256} required onChange={(event) => setForm({ ...form, modelName: event.target.value })} /></div>
      <div className="connection-form-wide"><label htmlFor="admin-model-base-url">服务地址</label><input id="admin-model-base-url" type="url" pattern="https://.*" value={form.baseURL} required onChange={(event) => setForm({ ...form, baseURL: event.target.value })} /><small className="muted">只接受 HTTPS；服务端还会执行出站地址校验。</small></div>
      <div className="connection-form-wide"><label htmlFor="admin-model-api-key">API 密钥</label><input id="admin-model-api-key" type="password" autoComplete="new-password" value={form.apiKey} maxLength={4096} required={!isEditing} onChange={(event) => setForm({ ...form, apiKey: event.target.value })} /><small className="muted">{isEditing ? '密钥不会回显；留空则保留已保存的密钥，输入新值才会替换。' : '密钥只提交给 Morde 服务端，保存后只显示“已配置”。'}</small></div>
    </div>
    {error && <p className="form-error" role="alert">{errorDetails(error).message}</p>}
    <div className="form-actions"><button type="button" className="button button-secondary" disabled={pending} onClick={onCancel}>取消</button><button type="submit" className="button button-primary" disabled={pending}>{pending ? '保存中…' : isEditing ? '保存连接' : '添加连接'}</button></div>
  </form>;
}

function capabilitySummary(capabilities: AdminModelConnection['capabilities']) {
  return `文本${capabilities.text ? '可用' : '不可用'} · 工具调用${capabilities.tools ? '可用' : '不可用'} · 视觉${capabilities.vision ? '可用' : '不可用'}`;
}
