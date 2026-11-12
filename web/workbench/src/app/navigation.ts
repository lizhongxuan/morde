type NativeWorkbenchBridge = { returnToPortalHosts?: () => Promise<unknown> | void };

export function returnToPreviousPage() {
  const bridge = (window as Window & { morde?: NativeWorkbenchBridge }).morde;
  if (typeof bridge?.returnToPortalHosts === 'function') {
    try {
      void Promise.resolve(bridge.returnToPortalHosts()).catch(() => {
        window.alert('无法返回云端电脑，请确认登录状态后重试。');
      });
    } catch {
      window.alert('无法返回云端电脑，请确认登录状态后重试。');
    }
    return;
  }
  window.alert('当前浏览器无法安全返回 Morde 云端电脑列表，请回到 Morde 客户端操作。');
}
