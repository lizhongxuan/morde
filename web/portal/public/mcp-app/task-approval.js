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
  const buttons = (approveLabel) => {
    const actions = document.createElement('div');
    actions.className = 'actions';
    for (const [decision, label] of [['reject', '拒绝'], ['approve', approveLabel]]) {
      const button = document.createElement('button');
      button.type = 'button';
      button.textContent = label;
      button.disabled = busy || !toolAvailable || decisionConfirmed || Boolean(attemptedDecision && attemptedDecision !== decision);
      button.addEventListener('click', () => decide(decision));
      actions.append(button);
    }
    return actions;
  };
  const reviewField = (label, value, className) => {
    const field = document.createElement('p');
    field.className = className;
    const name = document.createElement('span');
    name.className = 'review-field-label';
    name.textContent = label;
    const text = document.createElement('span');
    text.textContent = value;
    field.append(name, text);
    return field;
  };
  const reviewChip = (label, value, className = '') => {
    const chip = document.createElement('span');
    chip.className = `review-chip ${className}`;
    if (label) {
      const name = document.createElement('strong');
      name.textContent = label;
      chip.append(name);
    }
    const text = document.createElement('span');
    text.textContent = value;
    chip.append(text);
    return chip;
  };
  const renderReview = (notice) => {
    const card = document.createElement('div');
    card.className = 'review-card';
    const content = document.createElement('div');
    content.className = 'review-content';
    const heading = document.createElement('header');
    heading.className = 'review-heading';
    const shield = document.createElement('span');
    shield.className = 'review-shield';
    shield.setAttribute('aria-hidden', 'true');
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.setAttribute('fill', 'none');
    svg.setAttribute('stroke', 'currentColor');
    svg.setAttribute('stroke-width', '1.8');
    svg.setAttribute('stroke-linecap', 'round');
    svg.setAttribute('stroke-linejoin', 'round');
    for (const path of ['M12 3.2 5.5 5.8v5.1c0 4.2 2.7 7.6 6.5 9.5 3.8-1.9 6.5-5.3 6.5-9.5V5.8L12 3.2Z', 'm9.2 11.8 1.9 1.9 3.8-4']) {
      const element = document.createElementNS('http://www.w3.org/2000/svg', 'path');
      element.setAttribute('d', path);
      svg.append(element);
    }
    shield.append(svg);
    const title = document.createElement('h2');
    title.textContent = approval.title;
    const pending = document.createElement('span');
    pending.className = 'review-pending';
    pending.textContent = '待审批';
    heading.append(shield, title, pending);
    const operation = reviewField('操作', approval.content || '未提供', 'review-operation');
    const basis = reviewField('依据', `目标：${approval.object || '未提供'} · 影响：${approval.impact || '未提供'}`, 'review-basis');
    const meta = document.createElement('div');
    meta.className = 'review-meta';
    meta.setAttribute('aria-label', '风险、费用和授权范围');
    meta.append(reviewChip('风险', approval.risk, 'review-risk'),
      reviewChip('费用', approval.thisCost), reviewChip('', '仅本次操作', 'review-scope'));
    content.append(heading, operation, basis, meta);
    if ([approval.cumulativeCost, approval.alternatives, approval.revocation].some((value) => value && value !== '未知' && value !== '不适用')) {
      const details = document.createElement('details');
      details.className = 'review-more';
      const summary = document.createElement('summary');
      summary.textContent = '更多信息';
      const rows = document.createElement('div');
      rows.className = 'rows';
      rows.append(row('累计费用', approval.cumulativeCost), row('替代方案', approval.alternatives), row('撤销方式', approval.revocation));
      details.append(summary, rows);
      content.append(details);
    }
    const side = document.createElement('aside');
    side.className = 'review-side';
    side.setAttribute('aria-label', '审批操作');
    if (approval.expiresAt) {
      const expiry = document.createElement('div');
      expiry.className = 'review-expiry';
      const clock = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
      clock.setAttribute('viewBox', '0 0 16 16');
      clock.setAttribute('fill', 'none');
      clock.setAttribute('stroke', 'currentColor');
      clock.setAttribute('stroke-width', '1.3');
      clock.setAttribute('aria-hidden', 'true');
      const circle = document.createElementNS('http://www.w3.org/2000/svg', 'circle');
      circle.setAttribute('cx', '8');
      circle.setAttribute('cy', '8');
      circle.setAttribute('r', '6.1');
      const hands = document.createElementNS('http://www.w3.org/2000/svg', 'path');
      hands.setAttribute('d', 'M8 4.6v3.7l2.4 1.4');
      hands.setAttribute('stroke-linecap', 'round');
      clock.append(circle, hands);
      const time = document.createElement('time');
      time.dateTime = approval.expiresAt;
      time.textContent = new Intl.DateTimeFormat('zh-CN', {
        timeZone: 'Asia/Shanghai', month: 'long', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false,
      }).format(new Date(approval.expiresAt));
      expiry.append(clock, '到期', time);
      side.append(expiry);
    }
    const actions = buttons('允许本次');
    actions.classList.add('review-actions');
    const status = document.createElement('p');
    status.className = 'status';
    status.setAttribute('role', 'status');
    status.textContent = notice;
    side.append(actions, status);
    card.append(content, side);
    root.append(card);
  };
  const render = (notice = '') => {
    if (!approval || !root) return;
    root.replaceChildren();
    if (document.body.dataset.layout === 'review') {
      renderReview(notice);
      reportSize();
      return;
    }
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
    const actions = buttons('允许本次操作');
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
