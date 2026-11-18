import { useMemo, useState } from 'react';
import { useInfiniteQuery, useQuery } from '@tanstack/react-query';
import { formatBytes, formatDate } from '../app/format';
import { portalApi } from '../app/api';
import { ScreenPagination } from './ScreenPagination';

export function AgentWorkspaceBrowser({ active }: { active: boolean }) {
  const [location, setLocation] = useState({ workspaceID: '', path: '' });
  const workspace = useQuery({
    queryKey: ['my-agent-workspace'],
    queryFn: portalApi.myAgentWorkspace,
    enabled: active,
  });
  const path = workspace.data?.id === location.workspaceID ? location.path : '';
  const setPath = (value: string) => setLocation({ workspaceID: workspace.data?.id ?? '', path: value });

  const entries = useInfiniteQuery({
    queryKey: ['agent-workspace-entries', workspace.data?.id, path],
    queryFn: ({ pageParam }) => portalApi.agentWorkspaceEntries(workspace.data!.id, {
      ...(path ? { path } : {}), ...(pageParam ? { cursor: pageParam } : {}), limit: 100,
    }),
    initialPageParam: '',
    getNextPageParam: (page) => page.next_cursor || undefined,
    enabled: active && Boolean(workspace.data?.id),
    refetchInterval: active ? 5000 : false,
  });
  const items = useMemo(() => entries.data?.pages.flatMap((page) => page.entries) ?? [], [entries.data]);

  return <section className="agent-workspace-browser" aria-label="Agent Workspace 文件">
    <div className="agent-workspace-browser-heading">
      <h3>Agent Workspace</h3>
      <span>{path ? `~/workspace/${path}` : workspace.data?.root_path ?? '~/workspace'}</span>
    </div>
    {workspace.isPending && <p className="muted" role="status">正在读取云端 Agent 文件空间…</p>}
    {workspace.isError && <p className="error-inline" role="alert">云端 Agent 文件空间暂不可用。
      <button type="button" className="button button-small button-secondary" onClick={() => void workspace.refetch()}>重试</button>
    </p>}
    {workspace.data && <>
      {path && <button type="button" className="button button-small button-secondary agent-workspace-parent"
        onClick={() => setPath(path.includes('/') ? path.slice(0, path.lastIndexOf('/')) : '')}>‹ 上一级</button>}
      {entries.isPending && !entries.data && <p className="muted" role="status">正在读取目录…</p>}
      {entries.isError && <p className="error-inline" role="alert">目录读取失败。
        <button type="button" className="button button-small button-secondary" onClick={() => void entries.refetch()}>重试</button>
      </p>}
      {!entries.isPending && !entries.isError && items.length === 0 && <p className="muted">{path ? '此文件夹为空。' : '云端 Agent 文件空间为空。'}</p>}
      {items.length > 0 && <ScreenPagination items={items} getKey={(item) => item.path} estimatedItemHeight={72}
        height="100%" ariaLabel="Agent Workspace 文件列表" className="task-panel-pagination"
        renderItem={(item) => <article className="task-file-card agent-workspace-file-card" key={item.path}>
          <strong>{item.kind === 'directory' ? '文件夹 · ' : ''}{item.name}</strong>
          <small>{item.kind === 'file' ? formatBytes(item.size_bytes) : item.kind === 'symlink' ? '符号链接，不跟随读取' : item.kind === 'directory' ? '文件夹' : '其他项目'}
            {item.modified_at ? ` · ${formatDate(item.modified_at)}` : ''}</small>
          {item.kind === 'directory' && <button type="button" className="button button-small button-secondary"
            onClick={() => setPath(item.path)}>打开文件夹</button>}
          {item.kind === 'file' && <a href={portalApi.agentWorkspaceFileURL(workspace.data!.id, item.path)} download
            aria-label={`下载云端文件 ${item.path}`}>下载文件 <span aria-hidden="true">↓</span></a>}
        </article>} />}
      {entries.hasNextPage && <button type="button" className="button button-small button-secondary" disabled={entries.isFetchingNextPage}
        onClick={() => void entries.fetchNextPage()}>{entries.isFetchingNextPage ? '正在加载…' : '加载更多文件'}</button>}
    </>}
  </section>;
}
