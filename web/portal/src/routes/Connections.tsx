import { useQuery } from '@tanstack/react-query';
import { errorDetails, portalApi as api } from '../app/api';

type ModelConnection = Awaited<ReturnType<typeof api.modelConnections>>['items'][number];

export function Connections() {
  const models = useQuery({ queryKey: ['model-connections'], queryFn: api.modelConnections });
  const availableModels = models.data?.items.filter((model) => model.shared) ?? [];

  return <section>
    <div className="page-heading"><div><p className="eyebrow">接入与设置</p><h1>模型连接</h1><p className="muted">模型由管理员配置，个人 API Key 不在此页面收集。</p></div></div>
    <section className="connection-section" aria-labelledby="model-connections-title">
      <div className="connection-section-heading"><div><h2 id="model-connections-title">团队模型</h2><p className="muted">密钥由管理员安全保存，不会显示在列表或对话中。</p></div></div>
      {models.isPending ? <div className="loading-card" role="status">正在读取模型连接…</div>
        : models.isError ? <div className="error-card" role="alert">{errorDetails(models.error).message}<button type="button" className="button button-secondary" onClick={() => void models.refetch()}>重试</button></div>
          : !availableModels.length ? <div className="empty-card connection-empty"><h3>暂无可用模型</h3><p>请联系管理员配置团队模型。</p></div>
            : <div className="model-connection-list">{availableModels.map((model: ModelConnection) => <article className="model-connection-card" key={model.id}>
              <div className="model-connection-main"><div className="model-connection-title"><h3>{model.name}</h3><span className="status status-running">团队共享</span></div><p className="muted">{model.model_name} · {new URL(model.base_url).host}</p><div className="model-capabilities">{capabilityLabel('文本', model.capabilities.text)}{capabilityLabel('工具调用', model.capabilities.tools)}{capabilityLabel('视觉', model.capabilities.vision)}</div></div>
            </article>)}</div>}
    </section>
  </section>;
}

function capabilityLabel(label: string, available: boolean) {
  return <span className={`capability ${available ? 'capability-yes' : 'capability-no'}`} key={label}><span aria-hidden="true">{available ? '✓' : '–'}</span>{label}</span>;
}
