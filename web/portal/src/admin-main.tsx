import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { AdminApp } from './App';
import { disableDemoWorker } from './app/disable-demo-worker';
import './styles.css';

async function start() {
  if (import.meta.env.DEV && import.meta.env.VITE_MOCK_API === '1') {
    const { worker } = await import('./mocks/browser');
    await worker.start({ onUnhandledFrame: 'bypass' });
  } else if (await disableDemoWorker()) {
    window.location.reload();
    return;
  }

  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      <AdminApp />
    </StrictMode>,
  );
}

void start();
