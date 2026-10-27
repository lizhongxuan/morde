import { useState } from 'react';
import type { FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { components } from '../api/schema';
import { errorDetails, portalApi } from '../app/api';
import { useToast } from '../app/toast';
import { Modal } from '../components/Modal';

type ModelConnection = Awaited<ReturnType<typeof portalApi.modelConnections>>['items'][number];

export function Connections() {
  const client = useQueryClient();
  const { notify } = useToast();
  const models = useQuery({ queryKey: ['model-connections'], queryFn: portalApi.modelConnections });
  const [showForm, setShowForm] = useState(false);
  const [name, setName] = useState('');
  const [baseURL, setBaseURL] = useState('');
  const [modelName, setModelName] = useState('');
  const [apiKey, setApiKey] = useState('');
  const [checked, setChecked] = useState<{ id: string; capabilities: components['schemas']['ModelConnectionCapabilities'] } | null>(null);
  const create = useMutation({
    mutationFn: (body: components['schemas']['CreateModelConnectionRequest']) => portalApi.createModelConnection(body),
    onSuccess: async () => {
      setName(''); setBaseURL(''); setModelName(''); setApiKey(''); setShowForm(false);
      await client.invalidateQueries({ queryKey: ['model-connections'] });
      notify('模型连接已保存。', 'success');
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const check = useMutation({
    mutationFn: (id: string) => portalApi.checkModelConnection(id),
    onSuccess: (capabilities, id) => { setChecked({ id, capabilities }); notify('模型连接检查完成。', 'success'); void client.invalidateQueries({ queryKey: ['model-connections'] }); },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!name.trim() || !baseURL.trim() || !modelName.trim() || !apiKey || create.isPending) return;
    create.mutate({ name: name.trim(), base_url: baseURL.trim(), model_name: modelName.trim(), api_key: apiKey, shared: false });
  };
  const closeForm = () => { setShowForm(false); setApiKey(''); };

  return <section>
    <div className="page-heading"><div><p className="eyebrow">接入与设置</p><h1>模型连接</h1><p className="muted">管理 AI 模型连接。</p></div></div>
    <section className="connection-section" aria-labelledby="model-connections-title">
      <div className="connection-section-heading"><div><h2 id="model-connections-title">AI 模型</h2><p className="muted">模型密钥加密保存在服务端；保存后不会再次显示。</p></div><button type="button" className="button button-primary" onClick={() => setShowForm(true)}>＋ 添加模型</button></div>
      {showForm && <Modal title="添加个人模型" onClose={closeForm}><form className="connection-form" onSubmit={(event) => void submit(event)}>
        <div className="connection-form-grid">
          <div><label htmlFor="model-connection-name">显示名称</label><input id="model-connection-name" value={name} maxLength={80} onChange={(event) => setName(event.target.value)} placeholder="例如：团队主模型" required /></div>
          <div><label htmlFor="model-connection-model">模型名称</label><input id="model-connection-model" value={modelName} maxLength={256} onChange={(event) => setModelName(event.target.value)} placeholder="服务商提供的模型名称" required /></div>
          <div className="connection-form-wide"><label htmlFor="model-connection-url">服务地址</label><input id="model-connection-url" type="url" value={baseURL} onChange={(event) => setBaseURL(event.target.value)} placeholder="https://…" required /><small className="muted">仅接受 HTTPS。服务端只允许管理员配置的出站地址。</small></div>
          <div className="connection-form-wide"><label htmlFor="model-connection-key">API Key</label><input id="model-connection-key" type="password" autoComplete="new-password" value={apiKey} maxLength={4096} onChange={(event) => setApiKey(event.target.value)} required /><small className="muted">密钥只提交给 Morde 服务端，不会显示在列表或对话中。</small></div>
        </div>
        {create.isError && <p className="form-error" role="alert">{errorDetails(create.error).message}</p>}
        <div className="form-actions"><button type="button" className="button button-secondary" onClick={closeForm}>取消</button><button type="submit" className="button button-primary" disabled={create.isPending}>{create.isPending ? '保存中…' : '保存模型'}</button></div>
      </form></Modal>}
      {models.isPending ? <div className="loading-card" role="status">正在读取模型连接…</div>
        : models.isError ? <div className="error-card" role="alert">{errorDetails(models.error).message}<button type="button" className="button button-secondary" onClick={() => void models.refetch()}>重试</button></div>
          : !models.data.items.length ? <div className="empty-card connection-empty"><h3>暂无可用模型</h3><p>请联系管理员配置团队模型，或添加自己的模型连接。</p>{!showForm && <button type="button" className="button button-secondary" onClick={() => setShowForm(true)}>添加个人模型</button>}</div>
            : <div className="model-connection-list">{models.data.items.map((model: ModelConnection) => <article className="model-connection-card" key={model.id}>
              <div className="model-connection-main"><div className="model-connection-title"><h3>{model.name}</h3><span className={`status ${model.shared ? 'status-running' : 'status-queued'}`}>{model.shared ? '团队共享' : '个人'}</span></div><p className="muted">{model.model_name} · {new URL(model.base_url).host}</p><div className="model-capabilities">{capabilityLabel('文本', model.capabilities.text)}{capabilityLabel('工具调用', model.capabilities.tools)}{capabilityLabel('视觉', model.capabilities.vision)}</div>{checked?.id === model.id && <p className="notice notice-info model-check-result">检查结果：文本{checked.capabilities.text ? '可用' : '不可用'} · 工具调用{checked.capabilities.tools ? '可用' : '不可用'} · 视觉{checked.capabilities.vision ? '可用' : '不可用'}</p>}</div>
              <button type="button" className="button button-secondary" disabled={check.isPending} onClick={() => check.mutate(model.id)}>{check.isPending && check.variables === model.id ? '检查中…' : '检查连接'}</button>
            </article>)}</div>}
    </section>

  </section>;
}

function capabilityLabel(label: string, available: boolean) {
  return <span className={`capability ${available ? 'capability-yes' : 'capability-no'}`} key={label}><span aria-hidden="true">{available ? '✓' : '–'}</span>{label}</span>;
}
