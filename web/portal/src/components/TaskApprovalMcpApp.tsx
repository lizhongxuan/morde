import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';

export type McpApprovalDecision = 'approve' | 'reject';

export type McpApprovalAppData = {
  id: string;
  version: number;
  title: string;
  object: string;
  content: string;
  impact: string;
  risk: string;
  thisCost: string;
  cumulativeCost: string;
  alternatives: string;
  revocation: string;
  expiresAt: string | null;
};

type JsonRpcId = string | number;
type JsonRpcMessage = {
  jsonrpc?: unknown;
  id?: unknown;
  method?: unknown;
  params?: unknown;
};

const protocolVersion = '2026-01-26';
const approvalToolName = 'respond_to_task_approval';
const initializationTimeoutMs = 8_000;

function randomNonce(): string {
  const bytes = new Uint8Array(16);
  globalThis.crypto.getRandomValues(bytes);
  return Array.from(bytes, (value) => value.toString(16).padStart(2, '0')).join('');
}

function mcpAppDocument(nonce: string, parentOrigin: string, reviewInbox: boolean): string {
  const safeOrigin = parentOrigin.replaceAll('&', '&amp;').replaceAll('"', '&quot;').replaceAll('<', '&lt;');
  const safeNonce = nonce.replace(/[^a-f0-9]/gu, '');
  return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src ${safeOrigin}; style-src ${safeOrigin}; img-src 'none'; connect-src 'none'; font-src 'none'; object-src 'none'; form-action 'none'; base-uri 'none'"><link rel="stylesheet" href="${safeOrigin}/mcp-app/task-approval.css"><title>Morde 操作审批</title></head><body data-nonce="${safeNonce}" data-layout="${reviewInbox ? 'review' : 'conversation'}"><main id="app" aria-live="polite"></main><script src="${safeOrigin}/mcp-app/task-approval.js" defer></script></body></html>`;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function isRpcID(value: unknown): value is JsonRpcId {
  return typeof value === 'string' || (typeof value === 'number' && Number.isSafeInteger(value));
}

export function TaskApprovalMcpApp({ approval, onDecision, reviewInbox = false }: {
  approval: McpApprovalAppData;
  onDecision: (decision: McpApprovalDecision) => void | Promise<unknown>;
  reviewInbox?: boolean;
}) {
  const frameRef = useRef<HTMLIFrameElement>(null);
  const latestRef = useRef({ approval, onDecision });
  useLayoutEffect(() => {
    latestRef.current = { approval, onDecision };
  }, [approval, onDecision]);
  const [attempt, setAttempt] = useState(0);
  const [phase, setPhase] = useState({ attempt: 0, ready: false, failed: false });
  const [frameHeight, setFrameHeight] = useState(reviewInbox ? 170 : 310);
  const ready = phase.attempt === attempt && phase.ready;
  const failed = phase.attempt === attempt && phase.failed;
  const [nonce, setNonce] = useState(() => randomNonce());
  const initializedRef = useRef(false);
  const inFlightRef = useRef(false);
  const decisionRef = useRef<{ decision: McpApprovalDecision; confirmed: boolean } | null>(null);
  const hostRequestsRef = useRef(new Set<JsonRpcId>());
  const srcDoc = useMemo(() => mcpAppDocument(nonce, window.location.origin, reviewInbox), [nonce, reviewInbox]);

  useEffect(() => {
    initializedRef.current = false;
    inFlightRef.current = false;
    hostRequestsRef.current.clear();
    const timer = window.setTimeout(() => {
      if (!initializedRef.current) setPhase({ attempt, ready: false, failed: true });
    }, initializationTimeoutMs);
    const handleMessage = (event: MessageEvent) => {
      if (event.source !== frameRef.current?.contentWindow || event.origin !== 'null') return;
      if (!isRecord(event.data)) return;
      const message = event.data as JsonRpcMessage;
      if (message.jsonrpc !== '2.0' || typeof message.method !== 'string') return;
      const frame = frameRef.current?.contentWindow;
      if (!frame) return;
      const post = (response: Record<string, unknown>) => frame.postMessage(response, '*');
      const fail = (id: JsonRpcId, code: number, reason: string) =>
        post({ jsonrpc: '2.0', id, error: { code, message: reason } });

      if (message.method === 'ui/initialize' && isRpcID(message.id)) {
        const params = isRecord(message.params) ? message.params : {};
        const appInfo = isRecord(params.appInfo) ? params.appInfo : undefined;
        if (params.protocolVersion !== protocolVersion || !appInfo || typeof appInfo.name !== 'string' ||
          typeof appInfo.version !== 'string' || !isRecord(params.appCapabilities)) {
          fail(message.id, -32602, 'Invalid MCP App initialization');
          return;
        }
        initializedRef.current = true;
        setPhase({ attempt, ready: true, failed: false });
        post({ jsonrpc: '2.0', id: message.id, result: {
          protocolVersion,
          hostInfo: { name: 'Morde MCP Apps Host', version: '1.0.0' },
          hostCapabilities: { serverTools: {} },
          hostContext: { theme: 'light', displayMode: 'inline', platform: 'web' },
        } });
        return;
      }
      if (message.method === 'ui/notifications/initialized') {
        if (!initializedRef.current) return;
        post({ jsonrpc: '2.0', method: 'ui/notifications/tool-input', params: {
          arguments: latestRef.current.approval,
        } });
        return;
      }
      if (message.method === 'ui/notifications/size-changed') {
        const height = isRecord(message.params) ? message.params.height : undefined;
        if (initializedRef.current && typeof height === 'number' && Number.isFinite(height)) {
          setFrameHeight(Math.max(reviewInbox ? 120 : 310, Math.min(640, Math.ceil(height))));
        }
        return;
      }
      if (!isRpcID(message.id)) return;
      if (hostRequestsRef.current.has(message.id)) return;
      if (!initializedRef.current) {
        fail(message.id, -32002, 'MCP App initialization is incomplete');
        return;
      }
      hostRequestsRef.current.add(message.id);
      if (message.method === 'tools/list') {
        post({ jsonrpc: '2.0', id: message.id, result: { tools: [{
          name: approvalToolName,
          description: 'Respond to the currently displayed Morde task approval.',
          inputSchema: {
            type: 'object', additionalProperties: false,
            properties: {
              approval_id: { type: 'string' },
              version: { type: 'integer', minimum: 1 },
              decision: { type: 'string', enum: ['approve', 'reject'] },
            },
            required: ['approval_id', 'version', 'decision'],
          },
        }] } });
        return;
      }
      if (message.method !== 'tools/call') {
        fail(message.id, -32601, 'MCP App operation is unavailable');
        return;
      }
      const params = isRecord(message.params) ? message.params : {};
      const args = isRecord(params.arguments) ? params.arguments : undefined;
      const current = latestRef.current;
      const decision = args?.decision;
      if (params.name !== approvalToolName || !args || args.approval_id !== current.approval.id ||
        args.version !== current.approval.version || (decision !== 'approve' && decision !== 'reject')) {
        fail(message.id, -32602, 'Approval details changed; reload the card');
        return;
      }
      if (current.approval.expiresAt) {
        const expiresAt = Date.parse(current.approval.expiresAt);
        if (!Number.isFinite(expiresAt) || expiresAt <= Date.now()) {
          fail(message.id, -32000, 'Approval is no longer actionable');
          return;
        }
      }
      if (inFlightRef.current) {
        fail(message.id, -32002, 'An approval decision is already in progress');
        return;
      }
      if (decisionRef.current && (decisionRef.current.confirmed || decisionRef.current.decision !== decision)) {
        fail(message.id, -32002, 'Approval decision must be reconciled before another decision');
        return;
      }
      inFlightRef.current = true;
      decisionRef.current = { decision, confirmed: false };
      void Promise.resolve().then(() => current.onDecision(decision)).then(() => {
        decisionRef.current = { decision, confirmed: true };
        post({ jsonrpc: '2.0', id: message.id, result: {
          content: [{ type: 'text', text: '决定已提交；任务后续状态以服务端任务状态为准。' }],
          isError: false,
        } });
      }).catch(() => {
        post({ jsonrpc: '2.0', id: message.id, result: {
          content: [{ type: 'text', text: '审批结果尚未确认，请查看 Morde 对话中的状态。' }],
          isError: true,
        } });
      }).finally(() => {
        inFlightRef.current = false;
      });
    };
    window.addEventListener('message', handleMessage);
    return () => {
      window.clearTimeout(timer);
      window.removeEventListener('message', handleMessage);
    };
  }, [attempt, reviewInbox]);

  const reload = () => {
    initializedRef.current = false;
    setNonce(randomNonce());
    setAttempt((value) => value + 1);
  };

  return <section className="task-approval-mcp-app" aria-label="MCP App 操作审批" aria-busy={!ready && !failed}>
    {!ready && <p role="status">{failed ? '审批卡片加载失败，当前没有提交任何决定。' : '正在加载审批卡片…'}</p>}
    {failed && <button type="button" className="button button-secondary" onClick={reload}>重新加载审批卡片</button>}
    <iframe ref={frameRef} title="MCP App 操作审批" sandbox="allow-scripts" referrerPolicy="no-referrer"
      srcDoc={srcDoc} hidden={failed} style={{ height: frameHeight }} />
  </section>;
}
