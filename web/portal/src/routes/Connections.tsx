import { useState } from 'react';
import type { FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { UseMutationResult } from '@tanstack/react-query';
import { modelCheckErrorMessage } from '../api/errors';
import { errorDetails, portalApi as api } from '../app/api';
import { modelConnectionPresets } from '../app/model-presets';
import { useToast } from '../app/toast';
import { MCPConnectorsPanel } from './MCPConnectorsPanel';

type ModelConnection = Awaited<ReturnType<typeof api.modelConnections>>['items'][number];
type PersonalModelDraft = { name: string; baseURL: string; modelName: string; contextWindowTokens: string; apiKey: string };
const emptyDraft: PersonalModelDraft = { name: '', baseURL: '', modelName: '', contextWindowTokens: '', apiKey: '' };

export function Connections() {
  const client = useQueryClient();
  const { notify } = useToast();
  const models = useQuery({ queryKey: ['model-connections'], queryFn: api.modelConnections });
  const [formOpen, setFormOpen] = useState(false);
  const [editingID, setEditingID] = useState<string | null>(null);
  const [draft, setDraft] = useState<PersonalModelDraft>(emptyDraft);
  const [presetID, setPresetID] = useState('');
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<Error | null>(null);
  const [checked, setChecked] = useState<{ id: string; capabilities: ModelConnection['capabilities'] } | null>(null);
  const sharedModels = models.data?.items.filter((model) => model.shared) ?? [];
  const personalModels = models.data?.items.filter((model) => !model.shared) ?? [];
  const check = useMutation({
    mutationFn: (id: string) => api.checkModelConnection(id),
    onMutate: () => setChecked(null),
    onSuccess: async (capabilities, id) => {
      setChecked({ id, capabilities });
      await Promise.all([
        client.invalidateQueries({ queryKey: ['model-connections'] }),
        client.invalidateQueries({ queryKey: ['available-models'] }),
      ]);
      notify('个人模型检查完成。', 'success');
    },
    onError: () => { void refreshModels(); },
  });

  const refreshModels = async () => {
    await Promise.all([
      client.invalidateQueries({ queryKey: ['model-connections'] }),
      client.invalidateQueries({ queryKey: ['available-models'] }),
    ]);
  };

  const streamCheck = useMutation({
    mutationFn: (id: string) => api.checkModelTextStreaming(id),
    onSuccess: async (capabilities) => {
      await refreshModels();
      notify(capabilities.text_stream ? '模型流式能力已验证。' : '模型流式能力尚未验证，普通对话仍可用。', capabilities.text_stream ? 'success' : 'info');
    },
    onError: () => { void refreshModels(); },
  });

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (saving) return;
    const submittedDraft = draft;
    const selectedPresetID = presetID;
    setSaving(true);
    setDraft((current) => ({ ...current, apiKey: '' }));
    setSaveError(null);
    try {
      const connection = {
        name: submittedDraft.name.trim(), base_url: submittedDraft.baseURL.trim(), model_name: submittedDraft.modelName.trim(),
        ...(submittedDraft.contextWindowTokens ? { context_window_tokens: Number(submittedDraft.contextWindowTokens) } : {}),
      };
      const saved = editingID
        ? await api.updateModelConnection(editingID, { ...connection, ...(submittedDraft.apiKey ? { api_key: submittedDraft.apiKey } : {}) })
        : await api.createModelConnection({ ...connection, api_key: submittedDraft.apiKey, shared: false });
      setDraft(emptyDraft);
      setPresetID('');
      setFormOpen(false);
      setEditingID(null);
      setSaveError(null);
      setChecked(null);
      await refreshModels();
      if (!editingID && selectedPresetID) {
        try {
          await check.mutateAsync(saved.id);
        } catch (error) {
          notify(`个人模型已保存，但连接检查失败：${modelCheckErrorMessage(error)}`, 'error');
        }
      } else {
        notify(editingID ? '个人模型已更新。' : '个人模型已保存到 Morde 服务端。', 'success');
      }
    } catch (error) {
      setSaveError(error instanceof Error ? error : new Error('保存个人模型失败。'));
    } finally {
      setSaving(false);
    }
  };

  const cancelForm = () => {
    if (saving) return;
    setDraft(emptyDraft);
    setPresetID('');
    setEditingID(null);
    setSaveError(null);
    setFormOpen(false);
  };

  return <section className="connections-page">
    <div className="page-heading"><div><p className="eyebrow">接入与设置</p><h1>模型连接</h1><p className="muted">可使用管理员配置的共享模型，也可添加仅自己可见的模型。密钥只提交给 Morde 服务端，不会回显或保存在浏览器存储中。</p></div></div>
    {import.meta.env.DEV && import.meta.env.VITE_MOCK_API === '1' && <p className="notice notice-warning" role="status">当前为演示模式，没有连接真实服务。模型密钥不会保存到数据库，请勿输入真实 API 密钥。</p>}
    <section className="connection-section" aria-labelledby="model-connections-title">
      <div className="connection-section-heading"><div><h2 id="model-connections-title">团队模型</h2><p className="muted">由管理员配置，检查通过后可在 AI 助手中选择。</p></div></div>
      {models.isPending ? <div className="loading-card" role="status">正在读取模型连接…</div>
        : models.isError ? <div className="error-card" role="alert">{errorDetails(models.error).message}<button type="button" className="button button-secondary" onClick={() => void models.refetch()}>重试</button></div>
          : !sharedModels.length ? <div className="empty-card connection-empty"><h3>暂无团队模型</h3><p>管理员配置共享模型后会显示在这里。</p></div>
            : <div className="model-connection-list">{sharedModels.map((model: ModelConnection) => <ModelCard key={model.id} model={model} shared />)}</div>}
    </section>

    <section className="connection-section" aria-labelledby="personal-models-title">
      <div className="connection-section-heading"><div><h2 id="personal-models-title">我的模型</h2><p className="muted">个人模型只对当前账号开放，使用自己的 API 密钥，不占平台模型额度。请使用 HTTPS 地址；Morde 服务端会按当前域名 allowlist 校验出站连接。</p></div>
        <button type="button" className="button button-primary" onClick={() => { setDraft(emptyDraft); setPresetID(''); setEditingID(null); setFormOpen(true); setSaveError(null); check.reset(); }}>＋ 添加个人模型</button>
      </div>
      {formOpen && <form className="connection-form form-card" onSubmit={submit}>
        {editingID && <p className="muted">编辑个人模型。密钥不会回显；留空会保留已保存的密钥。修改模型、地址或密钥后需要重新检查。</p>}
        <div className="connection-form-grid">
          <div className="connection-form-wide"><label htmlFor="personal-model-preset">选择服务和模型</label><select id="personal-model-preset" value={presetID} onChange={(event) => {
            const preset = modelConnectionPresets.find((item) => item.id === event.target.value);
            setPresetID(preset?.id ?? '');
            if (preset) setDraft((current) => ({ ...current, name: preset.name, baseURL: preset.baseURL, modelName: preset.modelName }));
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
          </select><small className="muted">{editingID ? '选择后会自动填写显示名称、模型名和服务地址。' : '选择后会自动填写显示名称、模型名和服务地址；保存时会检查文本和工具调用能力。检查可能产生少量服务商费用。'}</small></div>
          <div><label htmlFor="personal-model-name">显示名称</label><input id="personal-model-name" value={draft.name} maxLength={80} required onChange={(event) => setDraft({ ...draft, name: event.target.value })} /></div>
          <div><label htmlFor="personal-model-provider-name">模型名</label><input id="personal-model-provider-name" value={draft.modelName} maxLength={256} required onChange={(event) => setDraft({ ...draft, modelName: event.target.value })} /></div>
          <div className="connection-form-wide"><label htmlFor="personal-model-context-window">上下文窗口（tokens）</label><input id="personal-model-context-window" type="number" min={4096} max={1000000} step={1} value={draft.contextWindowTokens} onChange={(event) => setDraft({ ...draft, contextWindowTokens: event.target.value })} /><small className="muted">按服务商模型文档填写。压缩需要此值；留空时会安全停用该模型的自动压缩。</small></div>
          <div className="connection-form-wide"><label htmlFor="personal-model-base-url">服务地址</label><input id="personal-model-base-url" type="url" pattern="https://.*" value={draft.baseURL} required onChange={(event) => setDraft({ ...draft, baseURL: event.target.value })} /><small className="muted">仅接受 HTTPS，并且必须通过服务端当前配置的域名 allowlist。</small></div>
          <div className="connection-form-wide"><label htmlFor="personal-model-api-key">API 密钥</label><input id="personal-model-api-key" type="password" autoComplete="new-password" value={draft.apiKey} maxLength={4096} required={!editingID} onChange={(event) => setDraft({ ...draft, apiKey: event.target.value })} /><small className="muted">{editingID ? '留空保留已保存的密钥，输入新值才会替换。' : '仅发送给服务端并加密保存。服务端不会返回密钥；不要在此输入共享密钥。'}</small></div>
        </div>
        {saveError && <p className="form-error" role="alert">{errorDetails(saveError).message}</p>}
        <div className="form-actions"><button type="button" className="button button-secondary" disabled={saving} onClick={cancelForm}>取消</button><button type="submit" className="button button-primary" disabled={saving}>{saving ? '正在保存…' : editingID ? '保存修改' : presetID ? '保存并检查连接' : '保存个人模型'}</button></div>
      </form>}
      {models.isPending ? null : models.isError ? null : !personalModels.length ? <div className="empty-card connection-empty"><h3>尚未添加个人模型</h3><p>添加后可以检查能力，并在 Assistant 的模型选项中选择。</p></div>
        : <div className="model-connection-list">{personalModels.map((model: ModelConnection) => <ModelCard key={model.id} model={model} checked={checked} check={check} streamCheck={streamCheck} onEdit={() => { setEditingID(model.id); setDraft({ name: model.name, baseURL: model.base_url, modelName: model.model_name, contextWindowTokens: model.context_window_tokens ? String(model.context_window_tokens) : '', apiKey: '' }); setPresetID(''); setSaveError(null); check.reset(); setFormOpen(true); }} />)}</div>}
    </section>
    <MCPConnectorsPanel />
  </section>;
}

function ModelCard({ model, shared, checked, check, streamCheck, onEdit }: {
  model: ModelConnection;
  shared?: boolean;
  checked?: { id: string; capabilities: ModelConnection['capabilities'] } | null;
  check?: UseMutationResult<ModelConnection['capabilities'], Error, string>;
  streamCheck?: UseMutationResult<ModelConnection['capabilities'], Error, string>;
  onEdit?: () => void;
}) {
  return <article className="model-connection-card">
    <div className="model-connection-main"><div className="model-connection-title"><h3>{model.name}</h3><span className={`status ${model.checked_at ? 'status-running' : 'status-stopped'}`}>{shared ? '团队共享' : '个人'}</span><span className="muted">{model.has_api_key ? '密钥已保存' : '未配置密钥'}</span></div>
      <p className="muted">{model.model_name} · {new URL(model.base_url).host}</p>
      <p className="muted">上下文窗口：{model.context_window_tokens ? `${model.context_window_tokens.toLocaleString()} tokens` : '未配置，暂不可压缩'}</p>
      {model.checked_at
        ? <div className="model-capabilities">{capabilityLabel('文本', model.capabilities.text)}{capabilityLabel('工具调用', model.capabilities.tools)}{capabilityLabel('视觉', model.capabilities.vision)}{capabilityLabel('流式能力', model.capabilities.text_stream)}</div>
        : <p className="muted">尚未检查；检查通过后可在 Assistant 的模型选项中选择。</p>}
      {checked?.id === model.id && <p className="notice notice-info model-check-result">本次检查：文本{checked.capabilities.text ? '可用' : '未验证'} · 工具调用{checked.capabilities.tools ? '可用' : '未验证'} · 视觉{checked.capabilities.vision ? '可用' : '未验证'}</p>}
      {check?.isError && check.variables === model.id && <p className="form-error" role="alert">{modelCheckErrorMessage(check.error)}</p>}
      {streamCheck?.isError && streamCheck.variables === model.id && <p className="form-error" role="alert">{modelCheckErrorMessage(streamCheck.error)}</p>}
      {!shared && <small className="muted">检查流式回复会发起一次模型调用，可能产生用量；请先检查文本能力。当前无工具的单任务可使用已验证的流式能力。</small>}
    </div>
    {!shared && <div className="inline-actions"><button type="button" className="button button-secondary button-small" disabled={check?.isPending || streamCheck?.isPending || !onEdit} onClick={onEdit}>编辑</button><button type="button" className="button button-secondary button-small" disabled={check?.isPending || streamCheck?.isPending} onClick={() => check?.mutate(model.id)}>{check?.isPending && check.variables === model.id ? '检查中…' : '检查连接'}</button><button type="button" className="button button-secondary button-small" disabled={check?.isPending || streamCheck?.isPending || !model.capabilities.text} onClick={() => streamCheck?.mutate(model.id)}>{streamCheck?.isPending && streamCheck.variables === model.id ? '检查中…' : '检查流式回复'}</button></div>}
  </article>;
}

function capabilityLabel(label: string, available: boolean) {
  return <span className={`capability ${available ? 'capability-yes' : 'capability-no'}`} key={label}><span aria-hidden="true">{available ? '✓' : '–'}</span>{label}</span>;
}
