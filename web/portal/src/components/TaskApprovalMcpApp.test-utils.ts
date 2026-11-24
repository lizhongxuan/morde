import { act } from '@testing-library/react';

let requestSequence = 0;

export function taskApprovalMcpAppDriver(frame: HTMLIFrameElement) {
  const nonce = frame.srcdoc.match(/data-nonce="([a-f0-9]+)"/u)?.[1];
  if (!nonce || !frame.contentWindow) throw new Error('Task approval MCP App frame is not initialized');
  const sent: unknown[] = [];
  Object.defineProperty(frame.contentWindow, 'postMessage', {
    configurable: true,
    value: (message: unknown) => sent.push(message),
  });
  const send = async (data: unknown, origin = 'null', source: MessageEventSource | null = frame.contentWindow) => {
    await act(async () => {
      window.dispatchEvent(new MessageEvent('message', { data, origin, source }));
      await Promise.resolve();
    });
  };
  const initialize = async () => {
    await send({ jsonrpc: '2.0', id: `initialize-${nonce}`, method: 'ui/initialize', params: {
      protocolVersion: '2026-01-26',
      appInfo: { name: 'test-task-approval', version: '1.0.0' },
      appCapabilities: {},
    } });
    await send({ jsonrpc: '2.0', method: 'ui/notifications/initialized', params: {} });
    await send({ jsonrpc: '2.0', id: `tools-${nonce}`, method: 'tools/list', params: {} });
  };
  const call = (id: string | number, arguments_: Record<string, unknown>, name = 'respond_to_task_approval') =>
    send({ jsonrpc: '2.0', id, method: 'tools/call', params: { name, arguments: arguments_ } });
  const currentApproval = () => {
    const message = sent.find((value) => {
      if (!value || typeof value !== 'object') return false;
      return (value as { method?: unknown }).method === 'ui/notifications/tool-input';
    }) as { params?: { arguments?: Record<string, unknown> } } | undefined;
    return message?.params?.arguments;
  };
  return { nonce, sent, send, initialize, call, currentApproval };
}

export async function submitTaskApprovalMcpAppDecision(frame: HTMLIFrameElement, decision: 'approve' | 'reject') {
  const driver = taskApprovalMcpAppDriver(frame);
  await driver.initialize();
  const approval = driver.currentApproval();
  if (!approval || typeof approval.id !== 'string' || !Number.isSafeInteger(approval.version)) {
    throw new Error('MCP App host did not send the current approval identity');
  }
  await driver.call(`call-${decision}-${++requestSequence}`, {
    approval_id: approval.id,
    version: approval.version,
    decision,
  });
  return driver;
}
