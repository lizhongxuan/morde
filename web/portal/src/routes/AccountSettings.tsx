import { useQuery } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { Link } from 'react-router';
import { errorDetails, portalApi } from '../app/api';
import { isTransientNetworkError } from '../api/errors';
import { useAuth } from '../app/auth';
import { formatBytes } from '../app/format';
import { useNetworkReadNotice } from '../app/use-network-read-notice';
import { FloatingError } from '../components/FloatingError';
import {
  subscribePersonalDataSyncStatus,
  type PersonalDataSyncStatus,
} from './personal-data-store';
import './AccountSettings.css';

type DesktopNotificationBridge = {
  desktopNotificationSettings?: () => Promise<unknown>;
  setDesktopNotificationsEnabled?: (enabled: boolean) => Promise<unknown>;
};

function desktopNotificationBridge(): DesktopNotificationBridge | undefined {
  if (typeof window === 'undefined') return undefined;
  return (window as Window & { morde?: DesktopNotificationBridge }).morde;
}

function DesktopNotificationSettings() {
  const bridge = desktopNotificationBridge();
  const loadPreference = bridge?.desktopNotificationSettings;
  const savePreference = bridge?.setDesktopNotificationsEnabled;
  const [enabled, setEnabled] = useState(false);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let current = true;
    if (!loadPreference || !savePreference) {
      setLoading(false);
      return () => { current = false; };
    }
    setLoading(true);
    setError(null);
    void loadPreference().then((value) => {
      const state = value && typeof value === 'object' ? value as { enabled?: unknown } : null;
      if (!state || typeof state.enabled !== 'boolean') throw new Error('Invalid desktop notification setting');
      if (current) setEnabled(state.enabled);
    }).catch(() => {
      if (current) setError('无法读取本机桌面提醒设置。');
    }).finally(() => {
      if (current) setLoading(false);
    });
    return () => { current = false; };
  }, [loadPreference, savePreference]);

  if (!loadPreference || !savePreference) return null;

  const toggle = async () => {
    if (saving || loading || error) return;
    setSaving(true);
    setError(null);
    try {
      const result = await savePreference(!enabled);
      const state = result && typeof result === 'object' ? result as { enabled?: unknown } : null;
      if (!state || typeof state.enabled !== 'boolean') throw new Error('Invalid desktop notification setting');
      setEnabled(state.enabled);
    } catch {
      setError('无法保存本机桌面提醒设置，请重试。');
    } finally {
      setSaving(false);
    }
  };

  return <section className="account-settings-info-block" aria-labelledby="desktop-notification-title">
    <div className="account-settings-toggle-row">
      <div>
        <h3 id="desktop-notification-title">任务完成桌面提醒</h3>
        <p>仅在 Morde 窗口未聚焦时尝试系统通知。通知不含任务标题或内容；点击后只打开 Assistant。系统通知不可用时，可在站内通知查看。</p>
      </div>
      <button type="button" role="switch" aria-label="任务完成桌面提醒" aria-checked={enabled} disabled={loading || saving || Boolean(error)}
        className="account-settings-switch" onClick={() => void toggle()}>
        <span aria-hidden="true" />
      </button>
    </div>
    {loading && <p role="status" className="notice notice-info">正在读取本机设置…</p>}
    {saving && <p role="status" className="notice notice-info">正在保存本机设置…</p>}
    {error && <p role="alert" className="notice notice-warning">{error}<button type="button" className="button button-secondary" onClick={() => {
      setError(null);
      setLoading(true);
      void loadPreference().then((value) => {
        const state = value && typeof value === 'object' ? value as { enabled?: unknown } : null;
        if (!state || typeof state.enabled !== 'boolean') throw new Error('Invalid desktop notification setting');
        setEnabled(state.enabled);
      }).catch(() => setError('无法读取本机桌面提醒设置。')).finally(() => setLoading(false));
    }}>重试</button></p>}
  </section>;
}

function PersonalSyncSettings() {
  const { user } = useAuth();
  const userID = user?.id ?? null;
  const capabilities = useQuery({ queryKey: ['service-capabilities'], queryFn: portalApi.capabilities });
  useNetworkReadNotice([{ failed: capabilities.isError, error: capabilities.error, retry: () => capabilities.refetch() }], '网络异常，服务状态暂时无法读取。');
  const serviceInstanceID = capabilities.data?.service_instance_id;
  const [storedStatus, setStoredStatus] = useState<PersonalDataSyncStatus & { userID: string | null }>(() => ({
    userID, pendingCount: null, conflictCount: null,
  }));
  const status = storedStatus.userID === userID ? storedStatus : {
    userID, pendingCount: null, conflictCount: null,
  };
  const quota = useQuery({ queryKey: ['personal-data-quota', serviceInstanceID], queryFn: portalApi.personalDataQuota,
    enabled: Boolean(serviceInstanceID), refetchInterval: serviceInstanceID ? 30000 : false });
  const quotaNetworkError = quota.isError && isTransientNetworkError(quota.error);
  useNetworkReadNotice([{ failed: quota.isError, error: quota.error, retry: () => quota.refetch() }], '网络异常，个人数据额度暂时无法读取。');

  useEffect(() => {
    if (!userID || !serviceInstanceID) return;
    const origin = window.location.origin;
    const unsubscribeStatus = subscribePersonalDataSyncStatus(userID, (next) => setStoredStatus({ ...next, userID }), origin, serviceInstanceID);
    return unsubscribeStatus;
  }, [userID, serviceInstanceID]);

  return <section className="account-settings-card" aria-labelledby="personal-data-title">
    <header className="account-settings-card-heading">
      <h2 id="personal-data-title">个人数据</h2>
      <p>管理个人资料同步状态与存储额度。</p>
    </header>
    <div className="account-settings-card-body">
      <section className="account-settings-info-block" aria-labelledby="personal-sync-title">
        <div>
          <h3 id="personal-sync-title">个人数据同步</h3>
          <p>保存后自动更新云端；手机连接后会获取最新资料。</p>
        </div>
        <p role="status" className="notice notice-info">{status.pendingCount === null || status.conflictCount === null ? '本机状态正在读取' : `待提交 ${status.pendingCount} 项 · 冲突 ${status.conflictCount} 项`}</p>
        {status.error && <FloatingError className="error-inline" role="alert">无法读取本机同步队列：{status.error}</FloatingError>}
        {status.conflictCount !== null && status.conflictCount > 0 && <p className="notice notice-warning">存在未解决冲突；本地草稿已保留，请到<Link to="/personal">个人数据</Link>查看并处理。</p>}
      </section>
      <section className="account-settings-info-block" aria-labelledby="personal-quota-title">
        <div>
          <h3 id="personal-quota-title">存储额度</h3>
          <p>包括个人对象、计划和个人附件，不包括聊天和云端电脑文件。</p>
        </div>
        {quota.isPending || (quotaNetworkError && !quota.data) ? <p className="notice notice-info" role="status">正在读取个人数据额度…</p>
          : quota.isError && !quotaNetworkError ? <FloatingError className="error-card" role="alert">{errorDetails(quota.error).message}<button type="button" className="button button-secondary" onClick={() => void quota.refetch()}>重试</button></FloatingError>
            : quota.data && <p className="notice notice-info" role="status">已用 {formatBytes(quota.data.used_bytes)}，待上传预留 {formatBytes(quota.data.reserved_bytes)}，总额度 {formatBytes(quota.data.quota_bytes)}，可用 {formatBytes(quota.data.remaining_bytes)}；单附件上限 {formatBytes(quota.data.max_attachment_bytes)}。</p>}
      </section>
    </div>
  </section>;
}

const roleLabels: Record<string, string> = {
  user: '普通用户', admin: '管理员', super_admin: '超级管理员',
};

const accountSections = [
  { id: 'account', title: '账号' },
  { id: 'personal-data', title: '个人数据' },
] as const;
type AccountSectionID = typeof accountSections[number]['id'];

export function AccountSettings() {
  const { user } = useAuth();
  const [activeSection, setActiveSection] = useState<AccountSectionID>('account');
  const [loadedSections, setLoadedSections] = useState<Set<AccountSectionID>>(() => new Set(['account']));
  const displayName = user?.name || '当前账号';

  return <section className="account-settings-page">
    <header className="account-settings-heading"><h1>账户设置</h1><p>管理账号安全、登录设备和个人数据。</p></header>
    <nav className="account-settings-tabs" role="tablist" aria-label="账户设置分区">
      {accountSections.map((item) => <button key={item.id} type="button" role="tab" id={`account-settings-tab-${item.id}`}
        aria-selected={activeSection === item.id} aria-controls={`account-settings-panel-${item.id}`}
        className="account-settings-tab" onClick={() => {
          setActiveSection(item.id);
          setLoadedSections((current) => current.has(item.id) ? current : new Set(current).add(item.id));
        }}>{item.title}</button>)}
    </nav>
    <section className="account-settings-panel" role="tabpanel" id="account-settings-panel-account" aria-labelledby="account-settings-tab-account" hidden={activeSection !== 'account'}>
      {loadedSections.has('account') && <section className="account-settings-card" aria-labelledby="account-info-title">
        <header className="account-settings-card-heading">
          <h2 id="account-info-title">账号与安全</h2>
          <p>查看账号信息并管理登录方式。</p>
        </header>
        <div className="account-settings-card-body">
          <div className="account-settings-account-row">
            <span className="account-settings-avatar" aria-hidden="true">{displayName.trim().slice(0, 1).toUpperCase()}</span>
            <div className="account-settings-account-copy">
              <div className="account-settings-name"><h3>{displayName}</h3><span className="status status-ready">{user ? roleLabels[user.role] ?? '已登录' : '加载中'}</span></div>
              <p>账号权限由服务端管理。</p>
            </div>
          </div>
          <div className="account-settings-action-row">
            <div><h3>登录密码</h3><p>修改密码后，其他设备需要重新登录。</p></div>
            <Link className="button button-secondary" to="/settings/password">修改密码</Link>
          </div>
          <div className="account-settings-action-row">
            <div><h3>已登录设备</h3><p>查看或撤销已登录的电脑和手机。</p></div>
            <Link className="button button-secondary" to="/clients">管理客户端</Link>
          </div>
          <DesktopNotificationSettings />
        </div>
      </section>}
    </section>
    <section className="account-settings-panel" role="tabpanel" id="account-settings-panel-personal-data" aria-labelledby="account-settings-tab-personal-data" hidden={activeSection !== 'personal-data'}>
      {loadedSections.has('personal-data') && <PersonalSyncSettings />}
    </section>
  </section>;
}
