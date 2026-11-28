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
type PersonalModelDraft = { name: string; baseURL: string; modelName: string; contextWindowOverrideTokens: string; clearContextWindowOverride: boolean; maxOutputOverrideTokens: string; clearMaxOutputOverride: boolean; apiKey: string };
type ConnectionArea = 'personal' | 'mcp';
const emptyDraft: PersonalModelDraft = { name: '', baseURL: '', modelName: '', contextWindowOverrideTokens: '', clearContextWindowOverride: false, maxOutputOverrideTokens: '', clearMaxOutputOverride: false, apiKey: '' };

function presetForRoute(baseURL: string, modelName: string) {
  return modelConnectionPresets.find((preset) => preset.baseURL.replace(/\/+$/, '') === baseURL.replace(/\/+$/, '') && preset.modelName === modelName);
}

function contextWindowSourceLabel(source: ModelConnection['context_window_source']) {
  switch (source) {
    case 'override': return '高级覆盖';
    case 'legacy_manual': return '旧手工配置';
    case 'provider_metadata': return '服务商元数据';
    case 'model_catalog': return 'Morde 模型目录';
    case 'provider_error': return '服务商错误响应';
    default: return '暂未识别';
  }
}

function outputLimitSourceLabel(source: ModelConnection['max_output_source']) {
  switch (source) {
    case 'override': return '高级覆盖';
    case 'model_catalog': return 'Morde 模型目录';
    default: return '暂未识别';
  }
}

function safeOutputSourceURL(value: string | null | undefined) {
  if (!value) return undefined;
  try {
    const url = new URL(value);
    return url.protocol === 'https:' && !url.username && !url.password ? url.toString() : undefined;
  } catch {
    return undefined;
  }
}

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
  const [formError, setFormError] = useState('');
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
      setFormError('API 密钥不能为空。');
      notify('API 密钥不能为空。', 'error');
      return;
    }
    setFormError('');
    setSaving(true);
    setDraft((current) => ({ ...current, apiKey: '' }));
    try {
      const isSystemPreset = Boolean(presetForRoute(submittedDraft.baseURL, submittedDraft.modelName));
      const connection = {
        name: submittedDraft.name.trim(), base_url: submittedDraft.baseURL.trim(), model_name: submittedDraft.modelName.trim(),
        ...(submittedDraft.clearContextWindowOverride ? { context_window_override_tokens: null }
          : !isSystemPreset && submittedDraft.contextWindowOverrideTokens ? { context_window_override_tokens: Number(submittedDraft.contextWindowOverrideTokens) } : {}),
        ...(submittedDraft.clearMaxOutputOverride ? { max_output_override_tokens: null }
          : !isSystemPreset && submittedDraft.maxOutputOverrideTokens ? { max_output_override_tokens: Number(submittedDraft.maxOutputOverrideTokens) } : {}),
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
      setFormError('');
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
      const message = `${outcomeUnknown ? '服务暂时不可用，保存结果尚未确认，请先核对模型列表。' : modelConnectionSaveErrorMessage(error)}${apiKey && !outcomeUnknown ? ' 密钥已清除，请重新输入后重试。' : ''}`;
      setFormError(message);
      notify(message, 'error');
    } finally {
      setSaving(false);
    }
  };

  const cancelForm = () => {
    if (saving) return;
    setDraft(emptyDraft);
    setPresetID('');
    setAdvancedOpen(false);
    setFormError('');
    setEditingID(null);
    setFormOpen(false);
  };

  const renderPersonalModel = (model: ModelConnection) => <ModelCard model={model} checked={checked} check={check} streamCheck={streamCheck} onEdit={() => {
    const preset = presetForRoute(model.base_url, model.model_name);
    setEditingID(model.id);
    setDraft({ name: model.name, baseURL: model.base_url, modelName: model.model_name,
      contextWindowOverrideTokens: preset ? '' : model.context_window_override_tokens ? String(model.context_window_override_tokens) : '',
      clearContextWindowOverride: false,
      maxOutputOverrideTokens: preset || model.max_output_override_tokens == null ? '' : String(model.max_output_override_tokens),
      clearMaxOutputOverride: false, apiKey: '' });
    setPresetID(preset?.id ?? 'custom');
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
            if (preset) setDraft((current) => ({ ...current, name: preset.name, baseURL: preset.baseURL, modelName: preset.modelName, contextWindowOverrideTokens: '', clearContextWindowOverride: false, maxOutputOverrideTokens: '', clearMaxOutputOverride: false }));
            else if (event.target.value === 'custom' && editingID) {
              const existing = personalModels.find((item) => item.id === editingID);
              setDraft((current) => ({ ...current,
                contextWindowOverrideTokens: existing?.context_window_override_tokens ? String(existing.context_window_override_tokens) : '', clearContextWindowOverride: false,
                maxOutputOverrideTokens: existing?.max_output_override_tokens == null ? '' : String(existing.max_output_override_tokens), clearMaxOutputOverride: false }));
            }
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
          {presetForRoute(draft.baseURL, draft.modelName) && <p className="muted connection-form-wide">系统预设按服务端返回信息显示上下文窗口和单次生成上限；未知时保持未知，无需手动填写。</p>}
          <div className="connection-form-wide"><label htmlFor="personal-model-api-key">API 密钥</label><input id="personal-model-api-key" type="password" autoComplete="new-password" value={draft.apiKey} maxLength={4096} required={!editingID} onChange={(event) => setDraft({ ...draft, apiKey: event.target.value })} /></div>
          <details className="connection-form-advanced connection-form-wide" open={advancedOpen} onToggle={(event) => {
            const details = event.currentTarget;
            if (!details.open && details.querySelector(':invalid')) details.open = true;
            setAdvancedOpen(details.open);
          }}>
            <summary>高级设置</summary>
            <div className="connection-form-grid connection-form-advanced-grid">
              <div><label htmlFor="personal-model-name">显示名称</label><input id="personal-model-name" value={draft.name} maxLength={80} required onChange={(event) => setDraft({ ...draft, name: event.target.value })} /></div>
              <div><label htmlFor="personal-model-provider-name">模型名</label><input id="personal-model-provider-name" value={draft.modelName} maxLength={256} required readOnly={presetID !== 'custom'} onChange={(event) => { setPresetID('custom'); setDraft({ ...draft, modelName: event.target.value, contextWindowOverrideTokens: '', clearContextWindowOverride: false, maxOutputOverrideTokens: '', clearMaxOutputOverride: false }); }} /></div>
              <div className="connection-form-wide"><label htmlFor="personal-model-base-url">服务地址</label><input id="personal-model-base-url" type="url" pattern="https://.*" value={draft.baseURL} required readOnly={presetID !== 'custom'} onChange={(event) => { setPresetID('custom'); setDraft({ ...draft, baseURL: event.target.value, contextWindowOverrideTokens: '', clearContextWindowOverride: false, maxOutputOverrideTokens: '', clearMaxOutputOverride: false }); }} /><small className="muted">使用 HTTPS，且服务域名须通过 Morde 服务端校验。</small></div>
              {!presetForRoute(draft.baseURL, draft.modelName) && <div className="connection-form-wide"><label htmlFor="personal-model-context-window">上下文窗口高级覆盖（tokens，可选）</label><input id="personal-model-context-window" type="number" min={4096} max={2000000} step={1} value={draft.contextWindowOverrideTokens} onChange={(event) => setDraft({ ...draft, contextWindowOverrideTokens: event.target.value, clearContextWindowOverride: false })} /><small className="muted">只用于自动发现不到容量的自定义服务；留空则保持未知，不使用通用默认值。</small></div>}
              {!presetForRoute(draft.baseURL, draft.modelName) && <div className="connection-form-wide"><label htmlFor="personal-model-max-output">单次生成上限高级覆盖（tokens，可选）</label><input id="personal-model-max-output" type="number" min={1} max={Number.MAX_SAFE_INTEGER} step={1} value={draft.maxOutputOverrideTokens} onChange={(event) => setDraft({ ...draft, maxOutputOverrideTokens: event.target.value, clearMaxOutputOverride: false })} /><small className="muted">只为自定义服务明确设置上限；留空保持当前值或未知状态，不按模型名推断。</small></div>}
              {!presetForRoute(draft.baseURL, draft.modelName) && editingID && personalModels.find((item) => item.id === editingID)?.max_output_override_tokens != null && <div className="connection-form-wide notice notice-info">
                <p>此自定义模型当前的单次生成上限覆盖为 {personalModels.find((item) => item.id === editingID)?.max_output_override_tokens?.toLocaleString()} tokens。</p>
                <button type="button" className="button button-secondary button-small" onClick={() => setDraft({ ...draft, clearMaxOutputOverride: true })} disabled={draft.clearMaxOutputOverride}>{draft.clearMaxOutputOverride ? '保存时将清除输出上限覆盖' : '清除覆盖，使用模型目录结果'}</button>
              </div>}
              {presetForRoute(draft.baseURL, draft.modelName) && editingID && personalModels.find((item) => item.id === editingID)?.context_window_override_tokens != null && <div className="connection-form-wide notice notice-info">
                <p>此系统预设保留了旧手工容量 {personalModels.find((item) => item.id === editingID)?.context_window_override_tokens?.toLocaleString()} tokens。</p>
                <button type="button" className="button button-secondary button-small" onClick={() => setDraft({ ...draft, clearContextWindowOverride: true })} disabled={draft.clearContextWindowOverride}>{draft.clearContextWindowOverride ? '保存时将清除旧值' : '清除旧手工值，使用 Morde 目录容量'}</button>
              </div>}
            </div>
          </details>
        </div>
        <div className="form-actions"><span><button type="button" className="button button-secondary" disabled={saving} onClick={cancelForm}>取消</button><button type="submit" className="button button-primary" disabled={saving}>{saving ? '正在保存…' : editingID ? '保存修改' : '保存并检查连接'}</button></span></div>
        {formError && <FloatingError inline className="error-inline" role="alert">{formError}</FloatingError>}
      </form></Modal>}
      {models.isError && models.data && <FloatingError inline className="error-card" role="alert">模型连接刷新失败，当前显示最近一次确认的连接。{errorDetails(models.error).message}<button className="button button-secondary" type="button" onClick={() => void models.refetch()}>重试</button></FloatingError>}
      {models.isPending && !models.data ? <div className="loading-card" role="status">正在读取模型连接…</div>
        : models.isError && !models.data ? <FloatingError inline className="error-card" role="alert">{errorDetails(models.error).message}<button className="button button-secondary" type="button" onClick={() => void models.refetch()}>重试</button></FloatingError>
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
      {check?.isError && check.variables === model.id && <FloatingError inline className="error-inline" role="alert">{modelCheckErrorMessage(check.error)}</FloatingError>}
      {streamCheck?.isError && streamCheck.variables === model.id && <FloatingError inline className="error-inline" role="alert">{modelCheckErrorMessage(streamCheck.error)}</FloatingError>}
    </div>
    {!shared && <div className="model-connection-actions"><button type="button" className="button button-secondary button-small" disabled={check?.isPending || streamCheck?.isPending || !onEdit} onClick={onEdit}>编辑</button><button type="button" className="button button-secondary button-small" disabled={check?.isPending || streamCheck?.isPending} onClick={() => check?.mutate(model.id)}>{check?.isPending && check.variables === model.id ? '检查中…' : '检查连接'}</button><button type="button" className="button button-secondary button-small" title={model.capabilities.text ? '检查流式回复会产生一次模型调用' : '先检查文本能力'} disabled={check?.isPending || streamCheck?.isPending || !model.capabilities.text} onClick={() => streamCheck?.mutate(model.id)}>{streamCheck?.isPending && streamCheck.variables === model.id ? '检查中…' : '检查流式回复'}</button></div>}
    <details className="model-connection-details">
      <summary>查看连接详情</summary>
      <div className="model-connection-detail-grid">
        <div><span>模型名</span><strong>{model.model_name}</strong></div>
        <div><span>服务商</span><strong>{modelHostLabel(model.base_url)}</strong></div>
        <div><span>API 密钥</span><strong>{shared ? '由管理员管理' : model.has_api_key ? '已保存' : '未配置'}</strong></div>
        <div><span>上下文窗口</span><strong>{model.context_window_tokens ? `${model.context_window_tokens.toLocaleString()} tokens` : '暂未识别'}</strong><small className="table-sub">来源：{contextWindowSourceLabel(model.context_window_source)}{model.context_window_catalog_version ? ` · 目录 ${model.context_window_catalog_version}` : ''}</small></div>
        <div><span>单次生成上限</span><strong>{model.max_output_tokens == null ? '暂未识别' : `${model.max_output_tokens.toLocaleString()} tokens`}</strong><small className="table-sub">来源：{outputLimitSourceLabel(model.max_output_source)}{model.max_output_catalog_version ? ` · 目录版本 ${model.max_output_catalog_version}` : ''}{safeOutputSourceURL(model.max_output_source_url) && <> · <a href={safeOutputSourceURL(model.max_output_source_url)} target="_blank" rel="noopener noreferrer">查看来源</a></>}</small></div>
      </div>
      {model.checked_at ? <div className="model-capabilities">{capabilityLabel('文本', model.capabilities.text)}{capabilityLabel('工具调用', model.capabilities.tools)}{capabilityLabel('视觉', model.capabilities.vision)}{capabilityLabel('流式能力', model.capabilities.text_stream)}</div> : <p className="muted">能力尚未检查。</p>}
      {!shared && <p className="muted model-connection-cost-note">检查连接或流式回复会发起模型调用，可能产生用量。</p>}
    </details>
  </article>;
}

function capabilityLabel(label: string, available: boolean) {
  return <span className={`capability ${available ? 'capability-yes' : 'capability-no'}`} key={label}><span aria-hidden="true">{available ? '✓' : '–'}</span>{label}</span>;
}
