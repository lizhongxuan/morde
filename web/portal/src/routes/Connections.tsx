import { useState } from 'react';
import type { FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { UseMutationResult } from '@tanstack/react-query';
import { isTransientNetworkError, modelCheckErrorMessage, modelConnectionSaveErrorMessage } from '../api/errors';
import { errorDetails, portalApi as api } from '../app/api';
import { modelConnectionPresets } from '../app/model-presets';
import { useToast } from '../app/toast';
import { useNetworkReadNotice } from '../app/use-network-read-notice';
import { Modal } from '../components/Modal';
import { MordeIcon } from '../components/MordeIcon';
import { ScreenPagination } from '../components/ScreenPagination';
import { MCPConnectorsPanel } from './MCPConnectorsPanel';
import { FloatingError, FloatingNotice } from '../components/FloatingError';

type ModelConnection = Awaited<ReturnType<typeof api.modelConnections>>['items'][number];
type PersonalModelDraft = { name: string; baseURL: string; modelName: string; contextWindowTokens: string; apiKey: string };
type ConnectionArea = 'personal' | 'mcp';
const emptyDraft: PersonalModelDraft = { name: '', baseURL: '', modelName: '', contextWindowTokens: '', apiKey: '' };

function modelHostLabel(value: string) {
  try {
    const url = new URL(value);
    return url.protocol === 'https:' && !url.username && !url.password ? url.host : '服务地址待核对';
  } catch {
    return '服务地址待核对';
  }
}

function connectionListHeight(itemCount: number) {
  return itemCount <= 2 ? 'min(38dvh, 440px)' : 'min(56dvh, 560px)';
}

export function Connections() {
  const client = useQueryClient();
  const { notify } = useToast();
  const models = useQuery({ queryKey: ['model-connections'], queryFn: api.modelConnections });
  useNetworkReadNotice([{ failed: models.isError, error: models.error, retry: models.refetch }], '模型连接状态读取失败，已保留当前内容。');
  const [activeArea, setActiveArea] = useState<ConnectionArea>('personal');
  const [formOpen, setFormOpen] = useState(false);
  const [editingID, setEditingID] = useState<string | null>(null);
  const [draft, setDraft] = useState<PersonalModelDraft>(emptyDraft);
  const [presetID, setPresetID] = useState('');
  const [advancedOpen, setAdvancedOpen] = useState(false);
  const [saving, setSaving] = useState(false);
  const [checked, setChecked] = useState<{ id: string; capabilities: ModelConnection['capabilities'] } | null>(null);
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
      notify(capabilities.tools ? '个人模型文本和工具调用均已验证。' : '文字对话可用；工具调用尚未验证。', capabilities.tools ? 'success' : 'info');
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
    const apiKey = submittedDraft.apiKey.trim();
    if (submittedDraft.apiKey && !apiKey) {
      notify('API 密钥不能为空。', 'error');
      return;
    }
    setSaving(true);
    setDraft((current) => ({ ...current, apiKey: '' }));
    try {
      const connection = {
        name: submittedDraft.name.trim(), base_url: submittedDraft.baseURL.trim(), model_name: submittedDraft.modelName.trim(),
        ...(submittedDraft.contextWindowTokens ? { context_window_tokens: Number(submittedDraft.contextWindowTokens) } : {}),
      };
      const saved = editingID
        ? await api.updateModelConnection(editingID, { ...connection, ...(apiKey ? { api_key: apiKey } : {}) })
        : await api.createModelConnection({ ...connection, api_key: apiKey, shared: false });
      setDraft(emptyDraft);
      setPresetID('');
      setAdvancedOpen(false);
      setFormOpen(false);
      setEditingID(null);
      setChecked(null);
      await refreshModels();
      if (!editingID) {
        try {
          await check.mutateAsync(saved.id);
        } catch (error) {
          notify(`个人模型已保存，但连接检查失败：${modelCheckErrorMessage(error)}`, 'error');
        }
      } else {
        notify('个人模型已更新。', 'success');
      }
    } catch (error) {
      const outcomeUnknown = isTransientNetworkError(error);
      void refreshModels();
      notify(`${outcomeUnknown ? '服务暂时不可用，保存结果尚未确认，请先核对模型列表。' : modelConnectionSaveErrorMessage(error)}${apiKey && !outcomeUnknown ? ' 密钥已清除，请重新输入后重试。' : ''}`, 'error');
    } finally {
      setSaving(false);
    }
  };

  const cancelForm = () => {
    if (saving) return;
    setDraft(emptyDraft);
    setPresetID('');
    setAdvancedOpen(false);
    setEditingID(null);
    setFormOpen(false);
  };

  const renderPersonalModel = (model: ModelConnection) => <ModelCard model={model} checked={checked} check={check} streamCheck={streamCheck} onEdit={() => {
    setEditingID(model.id);
    setDraft({ name: model.name, baseURL: model.base_url, modelName: model.model_name, contextWindowTokens: model.context_window_tokens ? String(model.context_window_tokens) : '', apiKey: '' });
    setPresetID('custom');
    setAdvancedOpen(true);
    check.reset();
    setFormOpen(true);
  }} />;

  const areaTabs = <div className="connection-area-tabs" role="group" aria-label="模型与工具分类">
    <button type="button" className={`button button-small ${activeArea === 'personal' ? 'button-primary' : 'button-secondary'}`} aria-pressed={activeArea === 'personal'} onClick={() => setActiveArea('personal')}>个人模型</button>
    <button type="button" className={`button button-small ${activeArea === 'mcp' ? 'button-primary' : 'button-secondary'}`} aria-pressed={activeArea === 'mcp'} onClick={() => setActiveArea('mcp')}>MCP 工具</button>
  </div>;

  return <section className="connections-page">
    {import.meta.env.DEV && import.meta.env.VITE_MOCK_API === '1' && <FloatingNotice className="notice-warning" data-notice-label="演示模式">当前为演示模式，没有连接真实服务。模型密钥不会保存到数据库，请勿输入真实 API 密钥。</FloatingNotice>}
    <section className="connection-section" aria-labelledby="personal-models-title" hidden={activeArea !== 'personal'}>
      <div className="connection-section-heading"><div><h2 id="personal-models-title">个人模型</h2><p className="muted">使用自己的 API 密钥，仅当前账号可见。</p></div>
        <button type="button" aria-label="添加个人模型" className="button button-primary" onClick={() => { setDraft(emptyDraft); setPresetID(''); setAdvancedOpen(false); setEditingID(null); setFormOpen(true); check.reset(); }}>＋ 添加模型</button>
      </div>
      {areaTabs}
      <h3 className="connections-list-title">模型连接</h3>
      {formOpen && <Modal title={editingID ? '编辑个人模型' : '添加个人模型'} onClose={cancelForm}><form className="connection-form connection-form-modal" onSubmit={submit}>
        <div className="connection-form-grid">
          <div className="connection-form-wide"><label htmlFor="personal-model-preset">选择服务和模型</label><select id="personal-model-preset" value={presetID} required onChange={(event) => {
            const preset = modelConnectionPresets.find((item) => item.id === event.target.value);
            setPresetID(event.target.value === 'custom' ? 'custom' : preset?.id ?? '');
            if (preset) setDraft((current) => ({ ...current, name: preset.name, baseURL: preset.baseURL, modelName: preset.modelName }));
            setAdvancedOpen(event.target.value === 'custom');
          }}>
            <option value="">选择服务商和模型</option>
            <option value="custom">自定义兼容模型</option>
            <optgroup label="OpenAI / GPT">
              {modelConnectionPresets.filter((preset) => preset.id.startsWith('gpt-')).map((preset) => <option key={preset.id} value={preset.id}>{preset.modelName}</option>)}
            </optgroup>
            <optgroup label="DeepSeek">
              {modelConnectionPresets.filter((preset) => preset.id.startsWith('deepseek-')).map((preset) => <option key={preset.id} value={preset.id}>{preset.modelName}</option>)}
            </optgroup>
            <optgroup label="Z.ai / GLM">
              {modelConnectionPresets.filter((preset) => preset.id.startsWith('glm-')).map((preset) => <option key={preset.id} value={preset.id}>{preset.modelName}</option>)}
            </optgroup>
          </select></div>
          <div className="connection-form-wide"><label htmlFor="personal-model-api-key">API 密钥</label><input id="personal-model-api-key" type="password" autoComplete="new-password" value={draft.apiKey} maxLength={4096} required={!editingID} onChange={(event) => setDraft({ ...draft, apiKey: event.target.value })} /></div>
          <details className="connection-form-advanced connection-form-wide" open={advancedOpen} onToggle={(event) => {
            const details = event.currentTarget;
            if (!details.open && details.querySelector(':invalid')) details.open = true;
            setAdvancedOpen(details.open);
          }}>
            <summary>高级设置</summary>
            <div className="connection-form-grid connection-form-advanced-grid">
              <div><label htmlFor="personal-model-name">显示名称</label><input id="personal-model-name" value={draft.name} maxLength={80} required onChange={(event) => setDraft({ ...draft, name: event.target.value })} /></div>
              <div><label htmlFor="personal-model-provider-name">模型名</label><input id="personal-model-provider-name" value={draft.modelName} maxLength={256} required onChange={(event) => setDraft({ ...draft, modelName: event.target.value })} /></div>
              <div className="connection-form-wide"><label htmlFor="personal-model-base-url">服务地址</label><input id="personal-model-base-url" type="url" pattern="https://.*" value={draft.baseURL} required onChange={(event) => setDraft({ ...draft, baseURL: event.target.value })} /><small className="muted">使用 HTTPS，且服务域名须通过 Morde 服务端校验。</small></div>
              <div className="connection-form-wide"><label htmlFor="personal-model-context-window">上下文窗口（tokens）</label><input id="personal-model-context-window" type="number" min={4096} max={1000000} step={1} value={draft.contextWindowTokens} onChange={(event) => setDraft({ ...draft, contextWindowTokens: event.target.value })} /><small className="muted">留空时不启用自动压缩。</small></div>
            </div>
          </details>
        </div>
        <div className="form-actions"><span><button type="button" className="button button-secondary" disabled={saving} onClick={cancelForm}>取消</button><button type="submit" className="button button-primary" disabled={saving}>{saving ? '正在保存…' : editingID ? '保存修改' : '保存并检查连接'}</button></span></div>
      </form></Modal>}
      {models.isPending || (!models.data && isTransientNetworkError(models.error)) ? <div className="loading-card" role="status">正在读取模型连接…</div>
        : models.isError && !isTransientNetworkError(models.error) ? <FloatingError className="error-card" role="alert">{errorDetails(models.error).message}<button className="button button-secondary" type="button" onClick={() => void models.refetch()}>重试</button></FloatingError>
          : !models.data ? <div className="loading-card" role="status">正在读取模型连接…</div>
          : !personalModels.length ? <><div className="connection-empty"><span className="connection-empty-icon" aria-hidden="true"><MordeIcon name="link" /></span><h3>尚未添加个人模型</h3><p>添加模型后，即可在对话中选择使用。</p></div><p className="connection-empty-note"><MordeIcon name="info" />模型配置和密钥仅对当前账号可见。</p></>
        : personalModels.length === 1 ? <div className="model-connection-list">{renderPersonalModel(personalModels[0]!)}</div>
        : <ScreenPagination
          items={personalModels}
          getKey={(model) => model.id}
          renderItem={renderPersonalModel}
          estimatedItemHeight={160}
          height={connectionListHeight(personalModels.length)}
          ariaLabel="个人模型"
          className="model-connection-list"
        />}
    </section>
    <div hidden={activeArea !== 'mcp'}><MCPConnectorsPanel areaTabs={areaTabs} /></div>
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
  const status = !model.checked_at ? 'pending' : model.capabilities.text ? 'passed' : 'failed';
  return <article className="model-connection-card">
    <div className="model-connection-main">
      <div className="model-connection-heading"><span className="model-provider-icon" aria-hidden="true">{model.model_name.trim().charAt(0).toUpperCase() || 'M'}</span><div className="model-connection-copy"><div className="model-connection-title"><h3>{model.name}</h3><span className={`model-connection-status model-connection-status-${status}`}><span aria-hidden="true" />{status === 'passed' ? '检查通过' : status === 'failed' ? '检查失败' : '尚未检查'}</span></div><p className="muted">{model.model_name} · {modelHostLabel(model.base_url)}</p></div></div>
      {checked?.id === model.id && <p className="notice notice-info model-check-result">本次检查：文本{checked.capabilities.text ? '可用' : '未验证'} · 工具调用{checked.capabilities.tools ? '可用' : '未验证'} · 视觉{checked.capabilities.vision ? '可用' : '未验证'}</p>}
      {check?.isError && check.variables === model.id && <FloatingError className="error-inline" role="alert">{modelCheckErrorMessage(check.error)}</FloatingError>}
      {streamCheck?.isError && streamCheck.variables === model.id && <FloatingError className="error-inline" role="alert">{modelCheckErrorMessage(streamCheck.error)}</FloatingError>}
    </div>
    {!shared && <div className="model-connection-actions"><button type="button" className="button button-secondary button-small" disabled={check?.isPending || streamCheck?.isPending || !onEdit} onClick={onEdit}>编辑</button><button type="button" className="button button-secondary button-small" disabled={check?.isPending || streamCheck?.isPending} onClick={() => check?.mutate(model.id)}>{check?.isPending && check.variables === model.id ? '检查中…' : '检查连接'}</button><button type="button" className="button button-secondary button-small" title={model.capabilities.text ? '检查流式回复会产生一次模型调用' : '先检查文本能力'} disabled={check?.isPending || streamCheck?.isPending || !model.capabilities.text} onClick={() => streamCheck?.mutate(model.id)}>{streamCheck?.isPending && streamCheck.variables === model.id ? '检查中…' : '检查流式回复'}</button></div>}
    <details className="model-connection-details">
      <summary>查看连接详情</summary>
      <div className="model-connection-detail-grid">
        <div><span>模型名</span><strong>{model.model_name}</strong></div>
        <div><span>服务商</span><strong>{modelHostLabel(model.base_url)}</strong></div>
        <div><span>API 密钥</span><strong>{shared ? '由管理员管理' : model.has_api_key ? '已保存' : '未配置'}</strong></div>
        <div><span>上下文窗口</span><strong>{model.context_window_tokens ? `${model.context_window_tokens.toLocaleString()} tokens` : '未配置，不启用自动压缩'}</strong></div>
      </div>
      {model.checked_at ? <div className="model-capabilities">{capabilityLabel('文本', model.capabilities.text)}{capabilityLabel('工具调用', model.capabilities.tools)}{capabilityLabel('视觉', model.capabilities.vision)}{capabilityLabel('流式能力', model.capabilities.text_stream)}</div> : <p className="muted">能力尚未检查。</p>}
      {!shared && <p className="muted model-connection-cost-note">检查连接或流式回复会发起模型调用，可能产生用量。</p>}
    </details>
  </article>;
}

function capabilityLabel(label: string, available: boolean) {
  return <span className={`capability ${available ? 'capability-yes' : 'capability-no'}`} key={label}><span aria-hidden="true">{available ? '✓' : '–'}</span>{label}</span>;
}
