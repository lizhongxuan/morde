export async function disableDemoWorker(): Promise<boolean> {
  if (!('serviceWorker' in navigator)) return false;
  const serviceWorkers = navigator.serviceWorker;
  const workerURL = new URL('/mockServiceWorker.js', window.location.origin).href;
  const rootScope = new URL('/', window.location.origin).href;
  const registrations = await serviceWorkers.getRegistrations();
  await Promise.all(registrations.filter((registration) => registration.scope === rootScope &&
    [registration.active, registration.waiting, registration.installing].some((worker) => worker?.scriptURL === workerURL))
    .map((registration) => registration.unregister()));
  return serviceWorkers.controller?.scriptURL === workerURL;
}
