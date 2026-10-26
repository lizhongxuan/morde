export const formatDate = (value?: string | null) => value
  ? new Intl.DateTimeFormat('zh-CN', { timeZone: 'Asia/Shanghai', dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value))
  : '—';

export const formatCpu = (milli?: number) => milli === undefined ? '—' : `${milli / 1000} 核`;
export const formatMemory = (mib?: number) => mib === undefined ? '—' : `${mib >= 1024 ? `${(mib / 1024).toFixed(mib % 1024 ? 1 : 0)} GiB` : `${mib} MiB`}`;
export const formatBytes = (bytes?: number) => bytes === undefined ? '—' : `${new Intl.NumberFormat('zh-CN').format(bytes)} B`;
export const formatDuration = (seconds: number) => `${Math.floor(seconds / 3600)} 小时`;

export const hostStateLabel: Record<string, string> = {
  creating: '创建中', running: '运行中', stopping: '停止中', stopped: '已停止',
  failed: '失败', deleting: '删除中', deleted: '已删除',
};
