(() => {
  const views = [...document.querySelectorAll('.view[id]')];
  const title = document.getElementById('crumb-title');
  const scrim = document.querySelector('.nav-scrim');
  const toast = document.getElementById('toast');
  const defaultView = document.body.dataset.defaultView;
  let toastTimer;

  function showToast(message) {
    toast.textContent = message;
    toast.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { toast.hidden = true; }, 3000);
  }

  function showView() {
    const id = decodeURIComponent(location.hash.slice(1));
    const current = views.find(view => view.id === id) || views.find(view => view.id === defaultView);
    if (!current) return;
    for (const view of views) view.hidden = view !== current;
    title.textContent = current.dataset.title || '设计预览';
    document.title = `${title.textContent} · Morde 设计稿`;
    const active = current.dataset.navParent || current.id;
    for (const link of document.querySelectorAll('.nav-link[href^="#"]')) {
      if (link.getAttribute('href') === `#${active}`) link.setAttribute('aria-current', 'page');
      else link.removeAttribute('aria-current');
    }
    document.querySelector('dialog[open]')?.close();
    document.body.classList.remove('nav-open');
    scrim.hidden = true;
    window.scrollTo({ top: 0, behavior: 'instant' });
  }

  document.querySelector('.mobile-menu')?.addEventListener('click', () => {
    const open = document.body.classList.toggle('nav-open');
    scrim.hidden = !open;
  });
  scrim?.addEventListener('click', () => {
    document.body.classList.remove('nav-open');
    scrim.hidden = true;
  });
  document.addEventListener('click', event => {
    const open = event.target.closest('[data-open-dialog]');
    if (open) document.getElementById(open.dataset.openDialog).showModal();
    if (event.target.closest('[data-close-dialog]')) event.target.closest('dialog')?.close();
    const button = event.target.closest('[data-demo]');
    if (button) showToast(button.dataset.demo || '这是设计稿演示，未连接真实服务。');
  });
  document.querySelectorAll('form[data-demo-action]').forEach(form => form.addEventListener('submit', event => {
    event.preventDefault();
    if (form.reportValidity()) {
      form.closest('dialog')?.close();
      showToast(form.dataset.demoAction || '这是设计稿演示，未保存数据。');
    }
  }));
  document.querySelectorAll('[data-filter]').forEach(input => input.addEventListener('input', () => {
    const table = document.querySelector(input.dataset.filter);
    if (!table) return;
    const query = input.value.trim().toLocaleLowerCase();
    for (const row of table.querySelectorAll('tbody tr')) row.hidden = !row.textContent.toLocaleLowerCase().includes(query);
  }));
  document.querySelectorAll('[data-tabs]').forEach(group => {
    const buttons = [...group.querySelectorAll('[data-tab]')];
    const panels = [...document.querySelectorAll(`[data-tab-panel^="${group.dataset.tabs}:"]`)];
    for (const button of buttons) button.addEventListener('click', () => {
      for (const item of buttons) item.setAttribute('aria-selected', String(item === button));
      for (const panel of panels) panel.hidden = panel.dataset.tabPanel !== `${group.dataset.tabs}:${button.dataset.tab}`;
    });
  });
  window.addEventListener('hashchange', showView);
  showView();
})();
