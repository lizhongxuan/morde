import { useQuery } from '@tanstack/react-query';
import { Link } from 'react-router';
import { formatBytes, formatDate } from '../app/format';
import { desktopCapabilityNames, getDesktopDownloads } from '../app/desktopDownloads';

const platformNames = {
  'macos:arm64': 'macOS · Apple 芯片',
  'macos:x64': 'macOS · Intel 芯片',
  'windows:x64': 'Windows · 64 位',
  'linux:x64': 'Linux · 64 位',
};

export function Downloads() {
  const downloads = useQuery({ queryKey: ['desktopDownloads'], queryFn: () => getDesktopDownloads() });

  return <main className="download-page">
    <div className="download-container">
      <header className="download-topbar">
        <Link className="brand" to="/login"><span className="brand-mark">M</span><span>Morde 云端电脑</span></Link>
        <Link className="button button-secondary" to="/login">登录</Link>
      </header>

      <section aria-labelledby="download-heading">
        <p className="eyebrow">MORDE DESKTOP</p>
        <h1 id="download-heading">下载 Morde Desktop</h1>
        <p className="download-intro">在电脑上选择本地项目文件夹，与 Morde 工作区同步；你的 AI 工具仍可使用本地文件。</p>
        {downloads.isPending && <p className="loading-card" role="status">正在读取可用版本…</p>}
        {downloads.isError && <div className="error-card" role="alert"><span>{downloads.error instanceof Error ? downloads.error.message : '暂时无法读取安装包列表'}</span><button className="button button-secondary" type="button" onClick={() => void downloads.refetch()}>重试</button></div>}
        {downloads.isSuccess && downloads.data.artifacts.length === 0 && <section className="empty-card download-empty"><h2>当前还没有可下载版本</h2><p>管理员发布桌面安装包后，这里会显示对应电脑的下载入口。</p></section>}
        {downloads.isSuccess && downloads.data.artifacts.length > 0 && <>
          <p className="download-version">版本 {downloads.data.version}</p>
          <div className="download-list">
            {downloads.data.artifacts.map((artifact) => {
              const label = platformNames[`${artifact.platform}:${artifact.architecture}` as keyof typeof platformNames];
              return <article className="download-card" key={`${artifact.platform}:${artifact.architecture}`}>
                <div className="download-card-copy">
                  <h2>{label}</h2>
                  <p>{artifact.filename} · {formatBytes(artifact.bytes)}</p>
                  <p>已验收：{artifact.acceptance.capabilities.map((capability) => desktopCapabilityNames[capability]).join('、')}</p>
                  <p>验收时间：{formatDate(artifact.acceptance.verified_at)} · {artifact.acceptance.signature_status === 'signed_and_notarized' ? '已签名并公证' : artifact.acceptance.signature_status === 'signed' ? '已签名' : '无需平台签名'}</p>
                  <details><summary>查看 SHA-256 校验值</summary><code>{artifact.sha256}</code></details>
                </div>
                <a className="button button-primary" href={artifact.download_url} download={artifact.filename} aria-label={`下载 ${label}`}>下载</a>
              </article>;
            })}
          </div>
          <p className="download-note">请按你的电脑芯片类型选择安装包。下载后按系统提示完成安装。</p>
        </>}
      </section>
    </div>
  </main>;
}
