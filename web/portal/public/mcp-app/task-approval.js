(() => {
  const nonce = document.body.dataset.nonce;
  const frame = window.parent;
  const requests = new Map();
  let nextID = 0;
  let hostOrigin;
  let approval;
  let toolAvailable = false;
  let busy = false;
  let attemptedDecision;
  let decisionConfirmed = false;
  const root = document.getElementById('app');

  const send = (message) => frame.postMessage(message, '*');
  const notify = (method, params = {}) => send({ jsonrpc: '2.0', method, params });
  const reportSize = () => {
    if (hostOrigin) notify('ui/notifications/size-changed', { height: Math.ceil(document.body.getBoundingClientRect().height) });
  };
  const request = (method, params, id) => new Promise((resolve, reject) => {
    requests.set(id, { resolve, reject });
    send({ jsonrpc: '2.0', id, method, params });
  });
  const row = (label, value) => {
    const item = document.createElement('div');
    item.className = 'row';
    const name = document.createElement('strong');
    name.textContent = label;
    const text = document.createElement('span');
    text.textContent = String(value ?? '未知');
    item.append(name, text);
    return item;
  };
  const render = (notice = '') => {
    if (!approval || !root) return;
    root.replaceChildren();
    const heading = document.createElement('h2');
    heading.textContent = approval.title;
    const object = document.createElement('p');
    object.className = 'object';
    object.textContent = approval.object;
    const content = document.createElement('p');
    content.textContent = approval.content;
    const impact = document.createElement('p');
    impact.className = 'impact';
    impact.textContent = `可能影响：${approval.impact}`;
    const details = document.createElement('details');
    const summary = document.createElement('summary');
    summary.textContent = '查看风险、费用和其他信息';
    const rows = document.createElement('div');
    rows.className = 'rows';
    rows.append(row('风险', approval.risk), row('本次费用', approval.thisCost),
      row('累计费用', approval.cumulativeCost), row('替代方案', approval.alternatives),
      row('撤销方式', approval.revocation));
    if (approval.expiresAt) rows.append(row('审批到期', approval.expiresAt));
    details.append(summary, rows);
    const explanation = document.createElement('p');
    explanation.className = 'explanation';
    explanation.textContent = '审批仅适用于本次显示的操作。允许后任务会继续尝试执行；拒绝后本次操作不会执行。最终结果以任务状态为准。';
    const status = document.createElement('p');
    status.className = 'status';
    status.setAttribute('role', 'status');
    status.textContent = notice;
    const actions = document.createElement('div');
    actions.className = 'actions';
    for (const [decision, label] of [['reject', '拒绝'], ['approve', '允许本次操作']]) {
      const button = document.createElement('button');
      button.type = 'button';
      button.textContent = label;
      button.disabled = busy || !toolAvailable || decisionConfirmed || Boolean(attemptedDecision && attemptedDecision !== decision);
      button.addEventListener('click', () => decide(decision));
      actions.append(button);
    }
    root.append(heading, object, content, impact, details, explanation, status, actions);
    reportSize();
  };
  if (root && typeof ResizeObserver !== 'undefined') new ResizeObserver(reportSize).observe(root);
  const decide = async (decision) => {
    if (!approval || busy || !toolAvailable || decisionConfirmed || (attemptedDecision && attemptedDecision !== decision)) return;
    attemptedDecision = decision;
    busy = true;
    render('正在提交决定…');
    const id = `call-${++nextID}`;
    try {
      const result = await request('tools/call', { name: 'respond_to_task_approval', arguments: {
        approval_id: approval.id, version: approval.version, decision,
      } }, id);
      if (result?.isError === true) throw new Error('Approval result was not confirmed');
      decisionConfirmed = true;
      render(decision === 'approve'
        ? '已提交允许决定；任务后续状态仍需确认。'
        : '已提交拒绝决定；任务后续状态仍需确认。');
    } catch {
      render('审批结果尚未确认。请核对状态，或重试相同决定。');
    } finally {
      busy = false;
      render(root?.querySelector('.status')?.textContent || '');
    }
  };

  window.addEventListener('message', (event) => {
    if (event.source !== frame || !event.data || typeof event.data !== 'object' || event.data.jsonrpc !== '2.0') return;
    const message = event.data;
    if (!hostOrigin) {
      if (event.origin !== 'null' || message.method !== 'ui/initialize' || message.id !== `initialize-${nonce}`) return;
      return;
    }
    if (event.origin !== hostOrigin) return;
    if (message.id !== undefined && requests.has(message.id)) {
      const pending = requests.get(message.id);
      requests.delete(message.id);
      if (message.error || message.result?.isError === true) pending.reject(new Error('Host request failed'));
      else pending.resolve(message.result);
      return;
    }
    if (message.method === 'ui/notifications/tool-input') {
      const args = message.params?.arguments;
      if (!args || (approval && args.id !== approval.id)) return;
      approval = args;
      render();
    }
  });

  const initializeID = `initialize-${nonce}`;
  requests.set(initializeID, {
    resolve: (result) => {
      const serverToolsAvailable = Boolean(result?.hostCapabilities?.serverTools);
      notify('ui/notifications/initialized');
      void request('tools/list', {}, `tools-${nonce}`).then((catalog) => {
        toolAvailable = serverToolsAvailable && Array.isArray(catalog?.tools) &&
          catalog.tools.some((tool) => tool.name === 'respond_to_task_approval');
        render(toolAvailable ? '' : '审批工具不可用，请刷新后重试。');
      }).catch(() => render('审批工具不可用，请刷新后重试。'));
    },
    reject: () => render('审批卡片初始化失败，请刷新后重试。'),
  });
  send({ jsonrpc: '2.0', id: initializeID, method: 'ui/initialize', params: {
    protocolVersion: '2026-01-26',
    appInfo: { name: 'morde-task-approval', title: 'Morde 操作审批', version: '1.0.0' },
    appCapabilities: {},
  } });

  const receiveInitializeResponse = (event) => {
    const message = event.data;
    if (event.source !== frame || message?.jsonrpc !== '2.0' || message.id !== initializeID) return;
    if (!event.origin || event.origin === 'null') return;
    hostOrigin = event.origin;
    window.removeEventListener('message', receiveInitializeResponse);
    const pending = requests.get(initializeID);
    requests.delete(initializeID);
    if (message.error) pending?.reject(new Error('MCP Apps host initialization failed'));
    else pending?.resolve(message.result);
  };
  window.addEventListener('message', receiveInitializeResponse);
})();
