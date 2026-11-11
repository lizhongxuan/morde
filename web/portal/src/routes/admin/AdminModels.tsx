import { useState } from 'react';
import type { FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { modelCheckErrorMessage } from '../../api/errors';
import { errorDetails, portalApi } from '../../app/api';
import { formatDate } from '../../app/format';
import { modelConnectionPresets } from '../../app/model-presets';
import { useToast } from '../../app/toast';
import { Modal } from '../../components/Modal';

type AdminModelConnection = Awaited<ReturnType<typeof portalApi.adminModelConnections>>['items'][number];
type ModelConnectionForm = { name: string; baseURL: string; modelName: string; contextWindowTokens: string; apiKey: string };

const emptyForm: ModelConnectionForm = { name: '', baseURL: '', modelName: '', contextWindowTokens: '', apiKey: '' };

export function AdminModels() {
  const client = useQueryClient();
  const { notify } = useToast();
  const query = useQuery({ queryKey: ['admin', 'model-connections'], queryFn: portalApi.adminModelConnections });
  const [formOpen, setFormOpen] = useState(false);
  const [formID, setFormID] = useState<string | null>(null);
  const [form, setForm] = useState<ModelConnectionForm>(emptyForm);
  const [presetID, setPresetID] = useState('');
  const [deleteID, setDeleteID] = useState<string | null>(null);
  const [checked, setChecked] = useState<{ id: string; capabilities: AdminModelConnection['capabilities'] } | null>(null);

  const refreshConnections = async () => {
    await client.invalidateQueries({ queryKey: ['admin', 'model-connections'] });
    void client.invalidateQueries({ queryKey: ['model-connections'] });
  };
  const check = useMutation({
    mutationFn: (id: string) => portalApi.checkModelConnection(id),
    onMutate: () => setChecked(null),
    onSuccess: async (capabilities, id) => {
      setChecked({ id, capabilities });
      await refreshConnections();
      notify('平台模型连接检查完成。', 'success');
    },
    onError: () => { void refreshConnections(); },
  });
  const streamCheck = useMutation({
    mutationFn: (id: string) => portalApi.checkModelTextStreaming(id),
    onSuccess: async (capabilities) => {
      await refreshConnections();
      notify(capabilities.text_stream ? '模型流式能力已验证。' : '模型流式能力尚未验证，普通对话仍可用。', capabilities.text_stream ? 'success' : 'info');
    },
    onError: () => { void refreshConnections(); },
  });
  const create = useMutation({
    mutationFn: ({ value }: { value: ModelConnectionForm; presetID: string }) => portalApi.createModelConnection({
      name: value.name.trim(), base_url: value.baseURL.trim(), model_name: value.modelName.trim(), api_key: value.apiKey, shared: true,
      ...(value.contextWindowTokens ? { context_window_tokens: Number(value.contextWindowTokens) } : {}),
    }),
    onSuccess: async (created, { presetID: selectedPresetID }) => {
      setFormOpen(false);
      setFormID(null);
      setForm(emptyForm);
      setPresetID('');
      if (selectedPresetID) {
        try {
          await check.mutateAsync(created.id);
        } catch (error) {
          await refreshConnections();
          notify(`平台模型已添加，但连接检查失败：${modelCheckErrorMessage(error)}`, 'error');
        }
      } else {
        await refreshConnections();
        notify('平台模型连接已添加。', 'success');
      }
    },
  });
  const update = useMutation({
    mutationFn: ({ id, value }: { id: string; value: ModelConnectionForm }) => portalApi.updateModelConnection(id, {
      name: value.name.trim(), base_url: value.baseURL.trim(), model_name: value.modelName.trim(),
      ...(value.contextWindowTokens ? { context_window_tokens: Number(value.contextWindowTokens) } : {}),
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
  function closeForm() {
    if (formMutation.isPending) return;
    setFormOpen(false);
    setFormID(null);
    setForm(emptyForm);
    setPresetID('');
  }

  function openCreate() {
    setFormOpen(true);
    setFormID(null);
    setForm(emptyForm);
    setPresetID('');
    create.reset();
    update.reset();
  }

  function openEdit(model: AdminModelConnection) {
    setFormOpen(true);
    setFormID(model.id);
    setForm({ name: model.name, baseURL: model.base_url, modelName: model.model_name,
      contextWindowTokens: model.context_window_tokens ? String(model.context_window_tokens) : '', apiKey: '' });
    setPresetID('');
    create.reset();
    update.reset();
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (formMutation.isPending) return;
    if (!form.name.trim() || !form.baseURL.trim() || !form.modelName.trim()) return;
    if (formID) update.mutate({ id: formID, value: form });
    else if (form.apiKey) create.mutate({ value: form, presetID });
  }

  const formMutation = formID ? update : create;
  const models = query.data?.items.filter((model) => model.shared) ?? [];

  return <section className="admin-content">
    <section className="design-panel" aria-labelledby="admin-model-list-title">
    <div className="design-panel-header"><div><h2 id="admin-model-list-title">共享模型连接</h2><p>管理平台模型连接和检查状态。</p></div>
      <button type="button" className="button button-primary" disabled={create.isPending || update.isPending} onClick={openCreate}>＋ 添加平台模型</button></div>

    {query.isPending ? <div className="loading-card" role="status">正在加载平台模型连接…</div>
      : query.isError ? <div className="error-card" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.refetch()}>重试</button></div>
        : !models.length ? <div className="empty-card compact"><h2>尚未配置平台模型</h2><p>可选择 GPT 或 DeepSeek 预设，也可以手动填写其他兼容服务。</p><button type="button" className="button button-secondary" onClick={openCreate}>添加平台模型</button></div>
          : <div className="table-wrap"><table><thead><tr><th>连接</th><th>服务地址</th><th>上下文窗口</th><th>密钥</th><th>能力 / 检查状态</th><th>更新时间</th><th>操作</th></tr></thead><tbody>
            {models.map((model) => <tr key={model.id}>
              <td><strong>{model.name}</strong><small className="table-sub">{model.model_name}</small></td>
              <td className="mono">{model.base_url}</td>
              <td>{model.context_window_tokens ? `${model.context_window_tokens.toLocaleString()} tokens` : '未配置'}</td>
              <td>{model.has_api_key ? '已配置' : '未配置'}</td>
              <td>{model.checked_at ? <><span>{capabilitySummary(model.capabilities)} · 流式能力{model.capabilities.text_stream ? '可用' : '未验证'}</span><small className="table-sub">已检查 · {formatDate(model.checked_at)}</small></> : <span className="muted">尚未检查</span>}
                {checked?.id === model.id && <p className="notice notice-info model-check-result">本次检查：{capabilitySummary(checked.capabilities)}</p>}
                {check.isError && check.variables === model.id && <p className="form-error" role="alert">{modelCheckErrorMessage(check.error)}</p>}
                {streamCheck.isError && streamCheck.variables === model.id && <p className="form-error" role="alert">{modelCheckErrorMessage(streamCheck.error)}</p>}
                <small className="table-sub">检查流式回复会发起一次模型调用，可能产生用量；请先检查文本能力。当前无工具的单任务可使用已验证的流式能力。</small>
              </td>
              <td>{formatDate(model.updated_at)}</td>
              <td><div className="inline-actions">
                <button type="button" className="button button-secondary button-small" disabled={streamCheck.isPending || check.isPending} onClick={() => openEdit(model)}>编辑</button>
                <button type="button" className="button button-secondary button-small" disabled={check.isPending || streamCheck.isPending} onClick={() => check.mutate(model.id)}>{check.isPending && check.variables === model.id ? '检查中…' : '检查连接'}</button>
                <button type="button" className="button button-secondary button-small" disabled={check.isPending || streamCheck.isPending || !model.capabilities.text} onClick={() => streamCheck.mutate(model.id)}>{streamCheck.isPending && streamCheck.variables === model.id ? '检查中…' : '检查流式回复'}</button>
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
    </section>

    {formOpen && <Modal title={formID === null ? '添加平台模型连接' : '编辑平台模型连接'} onClose={closeForm}><ModelConnectionFormView form={form} setForm={setForm} presetID={presetID} setPresetID={setPresetID} isEditing={formID !== null} onSubmit={submit} pending={formMutation.isPending} error={formMutation.error} onCancel={closeForm} /></Modal>}
  </section>;
}

function ModelConnectionFormView({ form, setForm, presetID, setPresetID, isEditing, onSubmit, pending, error, onCancel }: {
  form: ModelConnectionForm;
  setForm: (value: ModelConnectionForm) => void;
  presetID: string;
  setPresetID: (value: string) => void;
  isEditing: boolean;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
  pending: boolean;
  error: Error | null;
  onCancel: () => void;
}) {
  return <form className="connection-form" onSubmit={onSubmit}>
    <p className="muted">{isEditing ? '仅维护共享的平台连接。密钥不会回显；留空可保留当前密钥。修改模型地址后需要重新检查连接。' : '仅维护共享的平台连接。选择预设后填写 API 密钥即可；保存时会检查连接，以便设置为平台默认模型。'}</p>
    <div className="connection-form-grid">
      {!isEditing && <div className="connection-form-wide"><label htmlFor="admin-model-preset">选择服务和模型</label><select id="admin-model-preset" value={presetID} onChange={(event) => {
        const preset = modelConnectionPresets.find((item) => item.id === event.target.value);
        setPresetID(preset?.id ?? '');
        if (preset) setForm({ ...form, name: preset.name, baseURL: preset.baseURL, modelName: preset.modelName });
      }}>
        <option value="">选择预设，或手动填写兼容模型</option>
        <optgroup label="OpenAI / GPT">
          {modelConnectionPresets.filter((preset) => preset.id.startsWith('gpt-')).map((preset) => <option key={preset.id} value={preset.id}>{preset.modelName}</option>)}
        </optgroup>
        <optgroup label="DeepSeek">
          {modelConnectionPresets.filter((preset) => preset.id.startsWith('deepseek-')).map((preset) => <option key={preset.id} value={preset.id}>{preset.modelName}</option>)}
        </optgroup>
        <optgroup label="Z.ai / GLM">
          {modelConnectionPresets.filter((preset) => preset.id.startsWith('glm-')).map((preset) => <option key={preset.id} value={preset.id}>{preset.modelName}</option>)}
        </optgroup>
      </select><small className="muted">自动填写模型名和服务地址；连接检查验证文本和工具调用，可能产生少量服务商费用。</small></div>}
      <div><label htmlFor="admin-model-name">显示名称</label><input id="admin-model-name" value={form.name} maxLength={80} required onChange={(event) => setForm({ ...form, name: event.target.value })} /></div>
      <div><label htmlFor="admin-model-provider-name">模型名</label><input id="admin-model-provider-name" value={form.modelName} maxLength={256} required onChange={(event) => setForm({ ...form, modelName: event.target.value })} /></div>
      <div className="connection-form-wide"><label htmlFor="admin-model-context-window">上下文窗口（tokens）</label><input id="admin-model-context-window" type="number" min={4096} max={1000000} step={1} value={form.contextWindowTokens} onChange={(event) => setForm({ ...form, contextWindowTokens: event.target.value })} /><small className="muted">按服务商模型文档填写。压缩需要此值；留空时会安全停用该模型的自动压缩。</small></div>
      <div className="connection-form-wide"><label htmlFor="admin-model-base-url">服务地址</label><input id="admin-model-base-url" type="url" pattern="https://.*" value={form.baseURL} required onChange={(event) => setForm({ ...form, baseURL: event.target.value })} /><small className="muted">只接受 HTTPS；服务端还会执行出站地址校验。</small></div>
      <div className="connection-form-wide"><label htmlFor="admin-model-api-key">API 密钥</label><input id="admin-model-api-key" type="password" autoComplete="new-password" value={form.apiKey} maxLength={4096} required={!isEditing} onChange={(event) => setForm({ ...form, apiKey: event.target.value })} /><small className="muted">{isEditing ? '密钥不会回显；留空则保留已保存的密钥，输入新值才会替换。' : '密钥只提交给 Morde 服务端，保存后只显示“已配置”。'}</small></div>
    </div>
    {error && <p className="form-error" role="alert">{errorDetails(error).message}</p>}
    <div className="form-actions"><button type="button" className="button button-secondary" disabled={pending} onClick={onCancel}>取消</button><button type="submit" className="button button-primary" disabled={pending}>{pending ? '保存中…' : isEditing ? '保存连接' : presetID ? '添加并检查连接' : '添加连接'}</button></div>
  </form>;
}

function capabilitySummary(capabilities: AdminModelConnection['capabilities']) {
  return `文本${capabilities.text ? '可用' : '未验证'} · 工具调用${capabilities.tools ? '可用' : '未验证'} · 视觉${capabilities.vision ? '可用' : '未验证'}`;
}
