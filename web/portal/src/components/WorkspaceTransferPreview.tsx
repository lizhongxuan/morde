import { useId, useState } from 'react';

export type WorkspaceTransferPreviewSource = {
  id: string;
  name: string;
  revision: number;
  usedBytes: number;
  runtimeState: 'running' | 'stopped';
  checkpointReady: boolean;
  snapshotType: 'workspace' | 'host-runtime' | 'unsupported';
};

export type WorkspaceTransferPreviewProps = {
  source: WorkspaceTransferPreviewSource;
  ownerLabel?: string;
};

type CopyResult = {
  name: string;
  quotaBytes: number;
  sourceRevision: number;
};

type HandoffState = 'idle' | 'pending' | 'accepting' | 'canceling' | 'acceptance-failed'
  | 'accepted' | 'canceled' | 'failure-resolved';

const mebibyte = 1024 * 1024;

function formatBytes(bytes: number) {
  return `${new Intl.NumberFormat('zh-CN').format(bytes)} B`;
}

function transferFrozen(state: HandoffState) {
  return state === 'pending' || state === 'accepting' || state === 'canceling' || state === 'acceptance-failed';
}

export function WorkspaceTransferPreview({ source, ownerLabel = '当前所有者' }: WorkspaceTransferPreviewProps) {
  const id = useId();
  const [targetName, setTargetName] = useState(`${source.name} 的副本`);
  const [targetQuotaMiB, setTargetQuotaMiB] = useState(String(Math.max(1, Math.ceil(source.usedBytes / mebibyte))));
  const [acknowledged, setAcknowledged] = useState(false);
  const [copyResult, setCopyResult] = useState<CopyResult | null>(null);
  const [recipient, setRecipient] = useState('演示接收方');
  const [handoff, setHandoff] = useState<HandoffState>('idle');

  const targetQuotaBytes = Number(targetQuotaMiB) * mebibyte;
  const quotaSufficient = Number.isFinite(targetQuotaBytes) && targetQuotaBytes >= source.usedBytes;
  const snapshotAllowed = source.runtimeState === 'stopped'
    && source.checkpointReady
    && source.snapshotType === 'workspace'
    && quotaSufficient
    && targetName.trim().length > 0
    && acknowledged
    && !copyResult;
  const frozen = transferFrozen(handoff);
  const terminalHandoff = handoff === 'accepted' || handoff === 'canceled' || handoff === 'failure-resolved';
  const displayedOwner = handoff === 'accepted' ? recipient.trim() : ownerLabel;

  const snapshotTypeLabel = source.snapshotType === 'workspace' ? '普通 Workspace 文件' : source.snapshotType === 'host-runtime'
    ? 'Host Runtime 数据' : '未知内容类型';

  const createCopy = () => {
    if (!snapshotAllowed) return;
    setCopyResult({ name: targetName.trim(), quotaBytes: targetQuotaBytes, sourceRevision: source.revision });
  };

  return <div className="workspace-transfer-preview">
    <p className="notice notice-info" role="status">
      PC16 本地内存演示：不连接真实服务、不创建服务端工作区、不上传文件，也不会改变真实所有权。
    </p>

    <section className="form-card form-stack" aria-labelledby={`${id}-copy-heading`}>
      <h2 id={`${id}-copy-heading`}>工作区复制演示</h2>
      <div className="workspace-host-status">
        <strong>来源：{source.name || source.id}</strong>
        <small>版本 v{source.revision} · 占用 {formatBytes(source.usedBytes)}</small>
      </div>

      <div className="notice notice-warning" role="note">
        <strong>复制可能包含敏感内容。</strong>
        <p>快照会包含工作区内的隐藏文件（例如 .env、.git）和用户保存的凭据。请先检查内容并确认目标使用者可信。</p>
      </div>

      <label htmlFor={`${id}-target-name`}>新建独立 Workspace 的名称</label>
      <input id={`${id}-target-name`} value={targetName} maxLength={100} onChange={(event) => setTargetName(event.target.value)} />
      <label htmlFor={`${id}-target-quota`}>目标 Workspace 配额（MiB）</label>
      <input id={`${id}-target-quota`} type="number" min="0" step="1" value={targetQuotaMiB} onChange={(event) => setTargetQuotaMiB(event.target.value)} />

      <div className="workspace-host-status" aria-label="快照检查">
        <small>运行状态：{source.runtimeState === 'running' ? '运行中，禁止快照。' : '已停止。'}</small>
        <small>内容类型：{source.snapshotType === 'workspace' ? '受支持的 Workspace。' : `${snapshotTypeLabel}不支持复制。`}</small>
        <small>检查点：{source.checkpointReady ? '已就绪。' : '未就绪，暂不能快照。'}</small>
        <small>目标配额：{quotaSufficient ? '足够。' : `不足；复制需要 ${formatBytes(source.usedBytes)}。`}</small>
      </div>

      <label className="checkbox-row">
        <input type="checkbox" checked={acknowledged} onChange={(event) => setAcknowledged(event.target.checked)} />
        <span>我已检查快照可能包含的隐藏文件和用户保存凭据。</span>
      </label>
      <div className="form-actions">
        <button type="button" className="button button-primary" disabled={!snapshotAllowed} onClick={createCopy}>
          模拟快照并创建副本
        </button>
      </div>

      {copyResult && <div className="notice notice-success" role="status">
        <strong>已在页面内存中模拟创建独立 Workspace“{copyResult.name}”。</strong>
        <p>来源 v{copyResult.sourceRevision} 保持不变；目标配额 {formatBytes(copyResult.quotaBytes)}。这不是服务端副本。</p>
        <button type="button" className="button button-secondary button-small" onClick={() => { setCopyResult(null); setAcknowledged(false); }}>
          重置复制演示
        </button>
      </div>}
    </section>

    <section className="form-card form-stack" aria-labelledby={`${id}-handoff-heading`}>
      <h2 id={`${id}-handoff-heading`}>工作区移交演示</h2>
      <p className="muted">接受与取消竞争同一移交请求。锁定期间，双方都保持冻结；只有收到明确的服务端确认才会解锁。</p>
      <label htmlFor={`${id}-recipient`}>演示接收方</label>
      <input id={`${id}-recipient`} value={recipient} disabled={handoff !== 'idle' && !terminalHandoff}
        onChange={(event) => setRecipient(event.target.value)} />

      <div className="workspace-host-status" aria-label="移交双方冻结状态">
        <strong>来源工作区：{source.name || source.id}</strong>
        <small>当前所有者：{displayedOwner} · {frozen ? '已冻结（本地模拟）' : '未冻结'}</small>
        <small>接收方：{recipient.trim() || '未填写'} · {frozen ? '已冻结（本地模拟）' : '未冻结'}</small>
      </div>

      <div className="notice notice-info" role="status" aria-live="polite">
        {handoff === 'idle' && '尚未发起移交。'}
        {handoff === 'pending' && '移交请求已发起，双方已冻结；第一个接受或取消操作赢得本地竞争。'}
        {handoff === 'accepting' && '接受先赢得竞争；取消已失效。双方继续冻结，等待服务端确认。'}
        {handoff === 'canceling' && '取消先赢得竞争；接受已失效。双方继续冻结，等待服务端确认取消。'}
        {handoff === 'acceptance-failed' && '模拟接受失败。双方仍冻结；只有服务端确认双方状态未变后才能解锁。'}
        {handoff === 'accepted' && `服务端已确认移交给 ${recipient.trim()}；双方已解锁。`}
        {handoff === 'canceled' && '服务端已确认取消；所有权未变化，双方已解锁。'}
        {handoff === 'failure-resolved' && '服务端已确认所有权未变化并安全解锁。'}
      </div>

      {handoff === 'idle' && <div className="form-actions">
        <button type="button" className="button button-primary" disabled={!recipient.trim()} onClick={() => setHandoff('pending')}>
          开始移交演示并冻结双方
        </button>
      </div>}
      {handoff === 'pending' && <div className="form-actions">
        <button type="button" className="button button-primary" onClick={() => setHandoff('accepting')}>模拟接收方接受</button>
        <button type="button" className="button button-secondary" onClick={() => setHandoff('canceling')}>模拟所有者取消</button>
      </div>}
      {handoff === 'accepting' && <div className="form-actions">
        <button type="button" className="button button-primary" onClick={() => setHandoff('accepted')}>模拟服务端确认移交</button>
        <button type="button" className="button button-secondary" onClick={() => setHandoff('acceptance-failed')}>模拟接受失败</button>
      </div>}
      {handoff === 'canceling' && <div className="form-actions">
        <button type="button" className="button button-secondary" onClick={() => setHandoff('canceled')}>模拟服务端确认取消并解锁</button>
      </div>}
      {handoff === 'acceptance-failed' && <div className="form-actions">
        <button type="button" className="button button-secondary" onClick={() => setHandoff('failure-resolved')}>
          模拟服务端确认未变更并安全解锁
        </button>
      </div>}
      {terminalHandoff && <div className="form-actions">
        <button type="button" className="button button-secondary" onClick={() => setHandoff('idle')}>重置移交演示</button>
      </div>}
    </section>
  </div>;
}
