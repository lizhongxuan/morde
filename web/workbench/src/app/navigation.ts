export function returnToPreviousPage() {
  if (window.history.length > 1) { window.history.back(); return; }
  if (window.opener) { window.close(); return; }
  window.location.assign('/');
}
