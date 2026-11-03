import { useRef, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiError } from '../api/client';
import type { components } from '../api/schema';
import { errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import { useAuth } from '../app/auth';
import { ProjectMuseExtensions } from '../components/ProjectMuseExtensions';
import { projectApi } from '../app/project-api';
import type { AppendProjectConversationMessageResponse, Project, ProjectConversation, ProjectInvitation, ProjectMember, ProjectResource, ProjectRole, ProjectWorkItem } from '../app/project-api';

type RequestKeyRecord = { fingerprint: string; key: string };
type ActionNotice = { kind: 'success' | 'error' | 'unknown'; message: string };
type LedgerEntry = components['schemas']['ProjectBudgetLedgerEntry'];
type ResourceKind = components['schemas']['ProjectResourceKind'];
type ProjectAppendAttempt = {
  scope: string;
  key: string;
  projectID: string;
  conversationID: string;
  conversationOwnerID: string;
  body: components['schemas']['AppendProjectConversationMessageRequest'];
};
type ProjectTaskReceipt = { projectID: string; conversationID: string; value: AppendProjectConversationMessageResponse };

function definiteProjectAppendFailure(error: unknown) {
  if (!(error instanceof ApiError)) return false;
  return (error.status >= 400 && error.status < 500 && ![408, 425, 429].includes(error.status)) || error.status === 501;
}

function roleLabel(role: ProjectRole) {
  return ({ owner: '负责人', member: '成员', viewer: '只读成员' })[role];
}

function projectStateLabel(project: Project) {
  return `版本 v${project.version}`;
}

function invitationStateLabel(invitation: ProjectInvitation) {
  return ({ pending: '待接受', accepted: '已接受', revoked: '已撤回' })[invitation.state];
}

function workItemStateLabel(item: ProjectWorkItem) {
  return ({ open: '开放', blocked: '已阻塞', delivered: '已交付' })[item.state];
}

function safeMoney(value: number, currency: string) {
  if (!Number.isSafeInteger(value)) return '超出浏览器安全整数精度';
  const amount = BigInt(value);
  const whole = amount / 1_000_000n;
  const fraction = (amount % 1_000_000n).toString().padStart(6, '0').replace(/0+$/, '');
  return `${currency} ${whole.toLocaleString('zh-CN')}${fraction ? `.${fraction}` : ''}`;
}

function microsToInput(value: number) {
  if (!Number.isSafeInteger(value) || value < 0) return '';
  const amount = BigInt(value);
  const whole = amount / 1_000_000n;
  const fraction = (amount % 1_000_000n).toString().padStart(6, '0').replace(/0+$/, '');
  return `${whole}${fraction ? `.${fraction}` : ''}`;
}

function parseMicros(input: string): number | null {
  const match = /^(0|[1-9]\d*)(?:\.(\d{1,6}))?$/.exec(input.trim());
  if (!match) return null;
  const amount = BigInt(match[1]!) * 1_000_000n + BigInt((match[2] ?? '').padEnd(6, '0') || '0');
  if (amount < 1n || amount > BigInt(Number.MAX_SAFE_INTEGER)) return null;
  return Number(amount);
}

function usageLabel(entry: LedgerEntry) {
  if (entry.status === 'unknown') return '未知用量；费用尚未确认';
  if (!entry.actual_usage) return '尚无实际用量';
  return `输入 ${entry.actual_usage.input_tokens.toLocaleString('zh-CN')} tokens · 输出 ${entry.actual_usage.output_tokens.toLocaleString('zh-CN')} tokens`;
}

function displayResource(resource: ProjectResource) {
  return `${resource.kind === 'host' ? 'Host' : 'Workspace'} · ${resource.resource_id}`;
}

export function Projects() {
  const queryClient = useQueryClient();
  const { user } = useAuth();
  const [selectedProjectID, setSelectedProjectID] = useState('');
  const [projectName, setProjectName] = useState('');
  const [projectDescription, setProjectDescription] = useState('');
  const [inviteUserID, setInviteUserID] = useState('');
  const [inviteRole, setInviteRole] = useState<Exclude<ProjectRole, 'owner'>>('member');
  const [ownerTargetID, setOwnerTargetID] = useState('');
  const [resourceKind, setResourceKind] = useState<ResourceKind>('workspace');
  const [resourceID, setResourceID] = useState('');
  const [workspaceFileView, setWorkspaceFileView] = useState<{ projectID: string; workspaceID: string; revision?: number; cursor?: string } | null>(null);
  const [workTitle, setWorkTitle] = useState('');
  const [workDescription, setWorkDescription] = useState('');
  const [acceptanceCriteria, setAcceptanceCriteria] = useState('');
  const [blockReasons, setBlockReasons] = useState<Record<string, string>>({});
  const [taskChoices, setTaskChoices] = useState<Record<string, string>>({});
  const [deliveryChoices, setDeliveryChoices] = useState<Record<string, { taskID: string; workspaceID: string; revision: string }>>({});
  const [budgetCurrency, setBudgetCurrency] = useState('USD');
  const [budgetLimit, setBudgetLimit] = useState('10');
  const [projectConversationTitle, setProjectConversationTitle] = useState('');
  const [selectedProjectConversationID, setSelectedProjectConversationID] = useState('');
  const [projectPrompt, setProjectPrompt] = useState('');
  const [projectModelID, setProjectModelID] = useState('');
  const [projectHostID, setProjectHostID] = useState('');
  const [projectWorkspaceID, setProjectWorkspaceID] = useState('');
  const [uncertainProjectAppend, setUncertainProjectAppend] = useState<ProjectAppendAttempt | null>(null);
  const [projectTaskReceipt, setProjectTaskReceipt] = useState<ProjectTaskReceipt | null>(null);
  const [localOutgoingInvitations, setLocalOutgoingInvitations] = useState<Record<string, ProjectInvitation[]>>({});
  const [pendingAction, setPendingAction] = useState('');
  const [notice, setNotice] = useState<ActionNotice | null>(null);
  const requestKeys = useRef(new Map<string, RequestKeyRecord>());

  const projectsQuery = useQuery({ queryKey: ['projects'], queryFn: projectApi.projects, retry: false });
  const projects = projectsQuery.data ?? [];
  const projectID = selectedProjectID || projects[0]?.id || '';
  const projectTaskMock = import.meta.env.DEV && import.meta.env.VITE_MOCK_API === '1';
  const projectQuery = useQuery({ queryKey: ['project', projectID], queryFn: () => projectApi.getProject(projectID), enabled: Boolean(projectID), retry: false });
  const project = projectQuery.data;
  const membersQuery = useQuery({ queryKey: ['project-members', projectID], queryFn: () => projectApi.members(projectID), enabled: Boolean(projectID), retry: false });
  const members = membersQuery.data ?? [];
  const resourcesQuery = useQuery({ queryKey: ['project-resources', projectID], queryFn: () => projectApi.resources(projectID), enabled: Boolean(projectID), retry: false });
  const resources = resourcesQuery.data ?? [];
  const visibleWorkspaceFileView = workspaceFileView?.projectID === projectID
    && resources.some((resource) => resource.kind === 'workspace' && resource.resource_id === workspaceFileView.workspaceID)
    ? workspaceFileView : null;
  const workspaceManifestQuery = useQuery({
    queryKey: ['project-workspace-files', projectID, visibleWorkspaceFileView?.workspaceID, visibleWorkspaceFileView?.revision, visibleWorkspaceFileView?.cursor],
    queryFn: () => projectApi.workspaceManifest(projectID, visibleWorkspaceFileView!.workspaceID, {
      limit: 100,
      ...(visibleWorkspaceFileView?.revision !== undefined ? { revision: visibleWorkspaceFileView.revision } : {}),
      ...(visibleWorkspaceFileView?.cursor ? { cursor: visibleWorkspaceFileView.cursor } : {}),
    }),
    enabled: Boolean(projectID && visibleWorkspaceFileView), retry: false,
  });
  const workItemsQuery = useQuery({ queryKey: ['project-work-items', projectID], queryFn: () => projectApi.workItems(projectID), enabled: Boolean(projectID), retry: false });
  const workItems = workItemsQuery.data ?? [];
  const projectConversationsQuery = useQuery({ queryKey: ['project-conversations', projectID], queryFn: () => projectApi.projectConversations(projectID), enabled: Boolean(projectID), retry: false });
  const modelConnectionsQuery = useQuery({ queryKey: ['model-connections'], queryFn: portalApi.modelConnections, enabled: Boolean(projectID), retry: false });
  const taskHostsQuery = useQuery({ queryKey: ['project-task-hosts'], queryFn: portalApi.hosts, enabled: Boolean(projectID), retry: false });
  const taskWorkspacesQuery = useQuery({ queryKey: ['project-task-workspaces'], queryFn: portalApi.workspaces, enabled: Boolean(projectID), retry: false });
  const projectTaskCapabilitiesQuery = useQuery({ queryKey: ['service-capabilities'], queryFn: portalApi.capabilities, enabled: Boolean(projectID && !projectTaskMock), retry: false });
  const projectTaskExecutionReady = projectTaskMock || projectTaskCapabilitiesQuery.data?.supported_features.includes('project_budget_execution') === true;
  const budgetQuery = useQuery({ queryKey: ['project-budget', projectID], queryFn: () => projectApi.budget(projectID), enabled: Boolean(projectID), retry: false });
  const budget = budgetQuery.data;
  const ledgerQuery = useQuery({ queryKey: ['project-budget-ledger', projectID], queryFn: () => projectApi.ledger(projectID), enabled: Boolean(projectID), retry: false });
  const ledger = ledgerQuery.data ?? [];
  const invitationsQuery = useQuery({ queryKey: ['my-project-invitations'], queryFn: projectApi.myInvitations, retry: false });
  const invitations = invitationsQuery.data ?? [];
  const tasksQuery = useQuery({ queryKey: ['project-task-candidates'], queryFn: projectApi.tasks, enabled: Boolean(projectID), retry: false });
  const tasks = tasksQuery.data ?? [];
  const projectConversations = projectConversationsQuery.data ?? [];
  const selectedProjectConversation = projectConversations.find((item) => item.id === selectedProjectConversationID)
    ?? projectConversations[0];
  const projectConversationMessagesQuery = useQuery({
    queryKey: ['project-conversation-messages', projectID, selectedProjectConversation?.id],
    queryFn: () => projectApi.projectConversationMessages(projectID, selectedProjectConversation!.id),
    enabled: Boolean(projectID && selectedProjectConversation), retry: false,
  });
  const projectConversationMessages = projectConversationMessagesQuery.data ?? [];
  const readyTextModels = modelConnectionsQuery.data?.items.filter((model) => model.checked_at && model.capabilities.text) ?? [];
  const effectiveProjectModelID = readyTextModels.some((model) => model.id === projectModelID)
    ? projectModelID : readyTextModels[0]?.id ?? '';

  const currentMember = members.find((member) => member.user_id === user?.id);
  const isOwner = currentMember?.role === 'owner';
  const canEditWork = currentMember?.role === 'owner' || currentMember?.role === 'member';
  const sessionInvitations = localOutgoingInvitations[projectID] ?? [];
  const taskByID = new Map(tasks.map((task) => [task.id, task]));
  const projectHostIDs = new Set(resources.filter((resource) => resource.kind === 'host').map((resource) => resource.resource_id));
  const taskHosts = (taskHostsQuery.data?.items ?? []).filter((host) => projectHostIDs.has(host.id) && host.state === 'running' && host.control_state === 'ai');
  const selectedTaskHost = taskHosts.find((host) => host.id === projectHostID);
  const projectWorkspaceIDs = new Set(resources.filter((resource) => resource.kind === 'workspace').map((resource) => resource.resource_id));
  const taskWorkspaces = (taskWorkspacesQuery.data?.items ?? []).filter((workspace) => projectWorkspaceIDs.has(workspace.id)
    && workspace.state === 'ready' && Number.isSafeInteger(workspace.revision) && workspace.revision >= 0)
    .filter((workspace) => !selectedTaskHost || selectedTaskHost.workspaces.some((binding) => binding.workspace.id === workspace.id
      && binding.state === 'active' && binding.sync_state === 'healthy'
      && binding.desired_revision === workspace.revision && binding.applied_revision === workspace.revision));
  const selectedTaskWorkspace = taskWorkspaces.find((workspace) => workspace.id === projectWorkspaceID);
  const invalidTaskTargetSelection = Boolean(projectHostID && !selectedTaskHost) || Boolean(projectWorkspaceID && !selectedTaskWorkspace);
  const visibleProjectTaskReceipt = projectTaskReceipt?.projectID === projectID
    && projectTaskReceipt.conversationID === selectedProjectConversation?.id ? projectTaskReceipt : null;

  const invalidateProjectData = async () => Promise.all([
    queryClient.invalidateQueries({ queryKey: ['projects'] }),
    queryClient.invalidateQueries({ queryKey: ['project'] }),
    queryClient.invalidateQueries({ queryKey: ['project-members'] }),
    queryClient.invalidateQueries({ queryKey: ['project-resources'] }),
    queryClient.invalidateQueries({ queryKey: ['project-work-items'] }),
    queryClient.invalidateQueries({ queryKey: ['project-conversations'] }),
    queryClient.invalidateQueries({ queryKey: ['project-conversation-messages'] }),
    queryClient.invalidateQueries({ queryKey: ['project-budget'] }),
    queryClient.invalidateQueries({ queryKey: ['project-budget-ledger'] }),
    queryClient.invalidateQueries({ queryKey: ['my-project-invitations'] }),
    queryClient.invalidateQueries({ queryKey: ['project-task-candidates'] }),
  ]);

  const requestKeyFor = (scope: string, request: unknown) => {
    const fingerprint = JSON.stringify(request);
    const existing = requestKeys.current.get(scope);
    if (existing?.fingerprint === fingerprint) return existing.key;
    const key = newIdempotencyKey();
    requestKeys.current.set(scope, { fingerprint, key });
    return key;
  };

  const runAction = async <T,>(input: {
    action: string;
    run: () => Promise<T>;
    requestKey?: { scope: string; key: string };
    onSuccess?: (value: T) => void;
    successMessage: string;
  }) => {
    if (pendingAction) return;
    setPendingAction(input.action);
    setNotice(null);
    try {
      const value = await input.run();
      if (input.requestKey) requestKeys.current.delete(input.requestKey.scope);
      input.onSuccess?.(value);
      setNotice({ kind: 'success', message: input.successMessage });
    } catch (error) {
      const definite = error instanceof ApiError && (error.status < 500 || error.status === 501);
      if (definite && input.requestKey) requestKeys.current.delete(input.requestKey.scope);
      const message = errorDetails(error).message;
      setNotice({
        kind: definite ? 'error' : 'unknown',
        message: definite ? message : `结果尚未确认：${message}。已刷新项目数据，请以服务端返回状态为准；重试同一操作会复用请求编号。`,
      });
    } finally {
      await invalidateProjectData();
      setPendingAction('');
    }
  };

  const createProjectConversation = () => {
    if (!project || !currentMember) return;
    const title = projectConversationTitle.trim();
    const body = title ? { title } : {};
    const scope = `create-project-conversation:${project.id}`;
    const key = requestKeyFor(scope, body);
    void runAction({
      action: scope, run: () => projectApi.createProjectConversation(project.id, body, key), requestKey: { scope, key },
      onSuccess: (created) => {
        queryClient.setQueryData<ProjectConversation[]>(['project-conversations', project.id], (previous) => [
          created, ...(previous ?? []).filter((item) => item.id !== created.id),
        ]);
        setSelectedProjectConversationID(created.id);
        setProjectConversationTitle('');
        setProjectTaskReceipt(null);
      },
      successMessage: '服务端已创建项目共享对话；尚未启动 Task。',
    });
  };

  const runProjectAppend = async (attempt: ProjectAppendAttempt) => {
    if (pendingAction) return;
    setPendingAction(attempt.scope);
    setNotice(null);
    try {
      const value = await projectApi.appendProjectConversationMessage(attempt.projectID, attempt.conversationID, attempt.body, attempt.key);
      if (value.message.conversation_id !== attempt.conversationID || value.message.actor_id !== user?.id
        || value.message.owner_id !== attempt.conversationOwnerID || value.message.task_id !== value.task.id
        || value.task.project_id !== attempt.projectID || value.task.initiator_id !== user?.id
        || value.task.conversation_id !== attempt.conversationID) {
        throw new ApiError({ message: '服务端 Task 回执与提交的项目或发起人不一致', code: 'invalid_response', status: 502 });
      }
      requestKeys.current.delete(attempt.scope);
      setUncertainProjectAppend(null);
      setProjectTaskReceipt({ projectID: attempt.projectID, conversationID: attempt.conversationID, value });
      setProjectPrompt((current) => current.trim() === attempt.body.content ? '' : current);
      setNotice({ kind: 'success', message: `服务端已接受项目 Task ${value.task.id}；当前状态：${value.task.status}。` });
    } catch (error) {
      const definite = definiteProjectAppendFailure(error);
      if (definite) {
        requestKeys.current.delete(attempt.scope);
        setUncertainProjectAppend(null);
        setNotice({ kind: 'error', message: errorDetails(error).message });
      } else {
        setUncertainProjectAppend(attempt);
        setNotice({ kind: 'unknown', message: `项目 Task 结果尚未确认：${errorDetails(error).message}。重试会复用相同内容和幂等键。` });
      }
    } finally {
      await invalidateProjectData();
      setPendingAction('');
    }
  };

  const appendProjectPrompt = () => {
    const content = projectPrompt.trim();
    if (!project || !currentMember || !selectedProjectConversation || !effectiveProjectModelID || !content
      || !projectTaskExecutionReady || pendingAction || uncertainProjectAppend) return;
    const body: components['schemas']['AppendProjectConversationMessageRequest'] = {
      model_id: effectiveProjectModelID,
      content,
      ...(selectedTaskHost ? { host_id: selectedTaskHost.id } : {}),
      ...(selectedTaskWorkspace ? { workspace_id: selectedTaskWorkspace.id, input_revision: selectedTaskWorkspace.revision } : {}),
    };
    const scope = `append-project-prompt:${project.id}:${selectedProjectConversation.id}`;
    const attempt: ProjectAppendAttempt = {
      scope,
      key: requestKeyFor(scope, body),
      projectID: project.id,
      conversationID: selectedProjectConversation.id,
      conversationOwnerID: selectedProjectConversation.owner_id,
      body,
    };
    void runProjectAppend(attempt);
  };

  const retryProjectAppend = () => {
    // This is an exact idempotent replay of a request that may already have
    // created its Task. It must remain available even if readiness changes.
    if (uncertainProjectAppend) void runProjectAppend(uncertainProjectAppend);
  };

  const createProject = () => {
    const body = { name: projectName.trim(), ...(projectDescription.trim() ? { description: projectDescription.trim() } : {}) };
    if (!body.name) return;
    const scope = 'create-project';
    const key = requestKeyFor(scope, body);
    void runAction({
      action: 'create-project', run: () => projectApi.createProject(body, key), requestKey: { scope, key },
      onSuccess: (created) => { setSelectedProjectID(created.id); setProjectName(''); setProjectDescription(''); },
      successMessage: '服务端已确认创建项目。',
    });
  };

  const inviteMember = () => {
    if (!project || !inviteUserID.trim()) return;
    const body = { user_id: inviteUserID.trim(), role: inviteRole, expected_version: project.version };
    const scope = `invite:${project.id}`;
    const key = requestKeyFor(scope, body);
    void runAction({
      action: scope, run: () => projectApi.inviteMember(project.id, body, key), requestKey: { scope, key },
      onSuccess: (invitation) => {
        setLocalOutgoingInvitations((current) => ({ ...current, [project.id]: [...(current[project.id] ?? []).filter((item) => item.id !== invitation.id), invitation] }));
        setInviteUserID('');
      },
      successMessage: '服务端已创建待接受邀请；这不会发送邮件。',
    });
  };

  const acceptInvitation = (invitation: ProjectInvitation) => {
    const scope = `accept-invitation:${invitation.id}`;
    const key = requestKeyFor(scope, { invitation_id: invitation.id });
    void runAction({
      action: scope, run: () => projectApi.acceptInvitation(invitation.id, key), requestKey: { scope, key },
      onSuccess: (member) => setSelectedProjectID(member.project_id),
      successMessage: '服务端已确认接受邀请并建立成员关系。',
    });
  };

  const revokeOutgoingInvitation = (invitation: ProjectInvitation) => {
    if (!project) return;
    const body = { expected_version: invitation.version };
    const scope = `revoke-invitation:${invitation.id}`;
    const key = requestKeyFor(scope, body);
    void runAction({
      action: scope, run: () => projectApi.revokeInvitation(project.id, invitation.id, body, key), requestKey: { scope, key },
      onSuccess: () => setLocalOutgoingInvitations((current) => ({
        ...current, [project.id]: (current[project.id] ?? []).filter((item) => item.id !== invitation.id),
      })),
      successMessage: '服务端已确认撤回邀请。',
    });
  };

  const transferOwner = () => {
    if (!project || !ownerTargetID) return;
    const body = { user_id: ownerTargetID, expected_version: project.version };
    const scope = `transfer-owner:${project.id}`;
    const key = requestKeyFor(scope, body);
    void runAction({
      action: scope, run: () => projectApi.transferOwner(project.id, body, key), requestKey: { scope, key },
      successMessage: '服务端已确认负责人移交。',
    });
  };

  const updateMemberRole = (member: ProjectMember, role: Exclude<ProjectRole, 'owner'>) => {
    if (!project) return;
    const body = { role, expected_version: member.version };
    const scope = `update-member:${project.id}:${member.user_id}`;
    const key = requestKeyFor(scope, body);
    void runAction({
      action: scope, run: () => projectApi.updateMemberRole(project.id, member.user_id, body, key), requestKey: { scope, key },
      successMessage: `服务端已更新成员 ${member.user_id} 的角色。`,
    });
  };

  const removeMember = (member: ProjectMember) => {
    if (!project) return;
    const body = { expected_version: member.version };
    const scope = `remove-member:${project.id}:${member.user_id}`;
    const key = requestKeyFor(scope, body);
    void runAction({
      action: scope, run: () => projectApi.removeMember(project.id, member.user_id, body, key), requestKey: { scope, key },
      successMessage: `服务端已撤销成员 ${member.user_id} 的项目访问。`,
    });
  };

  const addResource = () => {
    if (!project || !resourceID.trim()) return;
    const body = { kind: resourceKind, resource_id: resourceID.trim(), expected_version: project.version };
    const scope = `add-resource:${project.id}:${resourceKind}:${resourceID.trim()}`;
    const key = requestKeyFor(scope, body);
    void runAction({
      action: scope, run: () => projectApi.addResource(project.id, body, key), requestKey: { scope, key },
      onSuccess: () => setResourceID(''), successMessage: '服务端已关联资源。关联不会增加 Host 或 Workspace 权限。',
    });
  };

  const removeResource = (resource: ProjectResource) => {
    if (!project) return;
    const body = { kind: resource.kind, resource_id: resource.resource_id, expected_version: project.version };
    const scope = `remove-resource:${project.id}:${resource.kind}:${resource.resource_id}`;
    const key = requestKeyFor(scope, body);
    void runAction({
      action: scope, run: () => projectApi.removeResource(project.id, body, key), requestKey: { scope, key },
      successMessage: '服务端已移除项目资源关联。资源本身和它的权限未改变。',
    });
  };

  const setProjectBudget = () => {
    if (!project || !budget) return;
    const micros = parseMicros(budgetLimit);
    const currency = budgetCurrency.trim().toUpperCase();
    if (micros === null || !/^[A-Z]{3}$/.test(currency)) return;
    const body = { currency, limit_micros: micros, expected_version: budget.version };
    const scope = `set-budget:${project.id}`;
    const key = requestKeyFor(scope, body);
    void runAction({
      action: scope, run: () => projectApi.setBudget(project.id, body, key), requestKey: { scope, key },
      onSuccess: (updated) => { setBudgetCurrency(updated.currency || currency); setBudgetLimit(microsToInput(updated.limit_micros)); },
      successMessage: '服务端已确认预算更新。',
    });
  };

  const createWorkItem = () => {
    if (!project || !workTitle.trim()) return;
    const body = {
      title: workTitle.trim(),
      ...(workDescription.trim() ? { description: workDescription.trim() } : {}),
      ...(acceptanceCriteria.trim() ? { acceptance_criteria: acceptanceCriteria.split('\n').map((line) => line.trim()).filter(Boolean) } : {}),
    };
    const scope = `create-work-item:${project.id}`;
    const key = requestKeyFor(scope, body);
    void runAction({
      action: scope, run: () => projectApi.createWorkItem(project.id, body, key), requestKey: { scope, key },
      onSuccess: () => { setWorkTitle(''); setWorkDescription(''); setAcceptanceCriteria(''); },
      successMessage: '服务端已创建工作项；没有因此启动 Task。',
    });
  };

  const assignWorkItem = (item: ProjectWorkItem, assigneeID: string) => {
    if (!project || !assigneeID || assigneeID === item.assignee_id) return;
    const body = { assignee_id: assigneeID, expected_version: item.version };
    const scope = `assign-work-item:${project.id}:${item.id}`;
    const key = requestKeyFor(scope, body);
    void runAction({
      action: scope, run: () => projectApi.assignWorkItem(project.id, item.id, body, key), requestKey: { scope, key },
      successMessage: '服务端已更新工作项负责人。',
    });
  };

  const blockWorkItem = (item: ProjectWorkItem) => {
    if (!project) return;
    const blockedReason = (blockReasons[item.id] ?? '').trim();
    if (!blockedReason) return;
    const body = { blocked_reason: blockedReason, expected_version: item.version };
    const scope = `block-work-item:${project.id}:${item.id}`;
    const key = requestKeyFor(scope, body);
    void runAction({
      action: scope, run: () => projectApi.blockWorkItem(project.id, item.id, body, key), requestKey: { scope, key },
      successMessage: '服务端已确认工作项进入阻塞状态。',
    });
  };

  const resumeWorkItem = (item: ProjectWorkItem) => {
    if (!project) return;
    const body = { expected_version: item.version };
    const scope = `resume-work-item:${project.id}:${item.id}`;
    const key = requestKeyFor(scope, body);
    void runAction({
      action: scope, run: () => projectApi.resumeWorkItem(project.id, item.id, body, key), requestKey: { scope, key },
      successMessage: '服务端已确认恢复工作项。',
    });
  };

  const linkTask = (item: ProjectWorkItem) => {
    if (!project) return;
    const taskID = taskChoices[item.id];
    if (!taskID) return;
    const body = { task_id: taskID, expected_version: item.version };
    const scope = `link-task:${project.id}:${item.id}`;
    const key = requestKeyFor(scope, body);
    void runAction({
      action: scope, run: () => projectApi.linkWorkItemTask(project.id, item.id, body, key), requestKey: { scope, key },
      successMessage: '服务端已确认 Task 关联。',
    });
  };

  const deliverWorkItem = (item: ProjectWorkItem) => {
    if (!project) return;
    const selected = deliveryChoices[item.id];
    const revision = Number(selected?.revision);
    if (!selected?.taskID || !selected.workspaceID.trim() || !Number.isSafeInteger(revision) || revision < 1) return;
    const body = { task_id: selected.taskID, workspace_id: selected.workspaceID.trim(), revision, expected_version: item.version };
    const scope = `deliver-work-item:${project.id}:${item.id}`;
    const key = requestKeyFor(scope, body);
    void runAction({
      action: scope, run: () => projectApi.deliverWorkItem(project.id, item.id, body, key), requestKey: { scope, key },
      successMessage: '服务端已确认交付证据并返回工作项。',
    });
  };

  const canSubmitBudget = budget !== undefined && parseMicros(budgetLimit) !== null && /^[A-Za-z]{3}$/.test(budgetCurrency.trim());
  const canManageWork = Boolean(project && canEditWork);

  return <section className="projects-page">
    <div className="page-heading"><div><p className="eyebrow">团队协作</p><h1>项目</h1><p className="muted">项目、成员、工作项与预算状态来自 Portal API。</p></div></div>
    <div className="notice notice-info" role="note">界面只按服务端响应更新；pending、blocked 和 unknown 会保留为服务端状态。工作项创建不会启动 Task；项目共享对话只有在明确提交提示后才排队。</div>
    {notice && <p className={`notice ${notice.kind === 'success' ? 'notice-success' : 'notice-warning'}`} role={notice.kind === 'error' ? 'alert' : 'status'}>{notice.message}</p>}

    <section className="connection-section" aria-labelledby="project-select-title">
      <div className="connection-section-heading"><div><h2 id="project-select-title">项目空间</h2><p className="muted">个人会话仍属于个人，不会随项目成员共享。</p></div></div>
      {projectsQuery.isPending ? <p role="status">正在读取项目…</p> : projectsQuery.isError ? <div className="error-card" role="alert">读取项目失败：{errorDetails(projectsQuery.error).message}<button type="button" className="button button-secondary" onClick={() => void projectsQuery.refetch()}>重试</button></div> : <>
        {projects.length > 0 && <div className="inline-form">
          <div><label htmlFor="project-select">当前项目</label><select id="project-select" value={projectID} disabled={Boolean(pendingAction || uncertainProjectAppend)} onChange={(event) => {
            setSelectedProjectID(event.target.value);
            setSelectedProjectConversationID('');
            setProjectHostID('');
            setProjectWorkspaceID('');
            setProjectTaskReceipt(null);
          }}>
            {projects.map((item) => <option value={item.id} key={item.id}>{item.name}</option>)}
          </select></div>
          <span className="muted">{project ? projectStateLabel(project) : '正在读取详情…'}</span>
        </div>}
        {!projects.length && <div className="empty-card compact"><p>当前账号还没有项目。</p></div>}
      </>}
      <form onSubmit={(event) => { event.preventDefault(); createProject(); }} className="form-stack">
        <label htmlFor="new-project-name">新项目名称</label>
        <input id="new-project-name" value={projectName} maxLength={120} onChange={(event) => setProjectName(event.target.value)} placeholder="例如：客户交付" required />
        <label htmlFor="new-project-description">项目说明</label>
        <textarea id="new-project-description" value={projectDescription} maxLength={2000} onChange={(event) => setProjectDescription(event.target.value)} rows={2} />
        <div className="form-actions"><button type="submit" className="button button-primary" disabled={!projectName.trim() || Boolean(pendingAction)}>{pendingAction === 'create-project' ? '正在创建…' : '创建项目'}</button></div>
      </form>
      {projectQuery.isError && <p className="form-error" role="alert">读取项目详情失败：{errorDetails(projectQuery.error).message}</p>}
    </section>

    <section className="connection-section" aria-labelledby="project-invitations-title">
      <div className="connection-section-heading"><div><h2 id="project-invitations-title">发给我的项目邀请</h2><p className="muted">邀请响应只提供项目 ID、邀请人 ID 和角色；服务端没有返回名称或邮箱。</p></div></div>
      {invitationsQuery.isPending ? <p role="status">正在读取邀请…</p> : invitationsQuery.isError ? <p className="form-error" role="alert">读取邀请失败：{errorDetails(invitationsQuery.error).message}</p> : !invitations.length ? <p className="muted">没有收到邀请。</p> : <div className="schedule-list">
        {invitations.map((invitation) => <article className="schedule-card" key={invitation.id}>
          <div className="schedule-card-heading"><div><h3>项目 {invitation.project_id}</h3><p className="muted">邀请人 {invitation.inviter_id} · {roleLabel(invitation.role)} · v{invitation.version}</p></div><span className="status">{invitationStateLabel(invitation)}</span></div>
          {invitation.state === 'pending' && <button type="button" className="button button-secondary" disabled={Boolean(pendingAction)} onClick={() => acceptInvitation(invitation)}>{pendingAction === `accept-invitation:${invitation.id}` ? '正在接受…' : '接受邀请'}</button>}
        </article>)}
      </div>}
    </section>

    {project && <>
      <section className="connection-section" aria-labelledby="project-members-title">
        <div className="connection-section-heading"><div><h2 id="project-members-title">成员与负责人</h2><p className="muted">接口只接收现有账号 ID；项目角色不自动授予 Host 或 Workspace 权限。</p></div></div>
        <p>当前负责人：<strong>{project.owner_id}</strong> · {projectStateLabel(project)}</p>
        <form onSubmit={(event) => { event.preventDefault(); inviteMember(); }} className="inline-form">
          <div><label htmlFor="invite-user-id">被邀请账号 ID</label><input id="invite-user-id" value={inviteUserID} onChange={(event) => setInviteUserID(event.target.value)} placeholder="u 加 7 位小写字母或数字" pattern="u[a-z0-9]{7}" maxLength={8} required /></div>
          <div><label htmlFor="invite-role">项目角色</label><select id="invite-role" value={inviteRole} onChange={(event) => setInviteRole(event.target.value as Exclude<ProjectRole, 'owner'>)}><option value="member">成员</option><option value="viewer">只读成员</option></select></div>
          <button type="submit" className="button button-secondary" disabled={!isOwner || !project || Boolean(pendingAction)}>{pendingAction === `invite:${project.id}` ? '正在邀请…' : '创建邀请'}</button>
        </form>
        {!isOwner && <p className="muted">只有当前负责人可以邀请、移除成员、改角色或移交负责人。</p>}
        {membersQuery.isPending ? <p role="status">正在读取成员…</p> : membersQuery.isError ? <p className="form-error" role="alert">读取成员失败：{errorDetails(membersQuery.error).message}</p> : <div className="schedule-list" aria-label="项目成员">
          {members.map((member) => <article className="schedule-card" key={member.user_id}>
            <div className="schedule-card-heading"><div><h3>{member.user_id}</h3><p className="muted">{roleLabel(member.role)} · 成员版本 v{member.version}</p></div><span className={`status ${member.role === 'owner' ? 'status-running' : ''}`}>{roleLabel(member.role)}</span></div>
            {isOwner && member.role !== 'owner' && <div className="schedule-actions">
              <label htmlFor={`role-${member.user_id}`}>角色</label>
              <select id={`role-${member.user_id}`} value={member.role} disabled={Boolean(pendingAction)} onChange={(event) => updateMemberRole(member, event.target.value as Exclude<ProjectRole, 'owner'>)}><option value="member">成员</option><option value="viewer">只读成员</option></select>
              <button type="button" className="button button-danger-quiet" disabled={Boolean(pendingAction)} onClick={() => removeMember(member)}>撤销成员</button>
            </div>}
          </article>)}
          {!members.length && <p className="muted">服务端尚未返回成员。</p>}
        </div>}

        {isOwner && <div className="inline-form">
          <div><label htmlFor="owner-transfer-target">移交负责人给已接受成员</label><select id="owner-transfer-target" value={ownerTargetID} onChange={(event) => setOwnerTargetID(event.target.value)}>
            <option value="">选择成员</option>{members.filter((member) => member.role !== 'owner').map((member) => <option key={member.user_id} value={member.user_id}>{member.user_id}（{roleLabel(member.role)}）</option>)}
          </select></div>
          <button type="button" className="button button-secondary" disabled={!ownerTargetID || Boolean(pendingAction)} onClick={transferOwner}>{pendingAction === `transfer-owner:${project.id}` ? '正在移交…' : '移交负责人'}</button>
        </div>}

        <div className="schedule-list" aria-label="本次页面创建的待处理邀请">
          <h3>本次页面创建的待处理邀请</h3>
          <p className="muted">OpenAPI 没有按项目列出已发邀请的 GET 路由。这里仅显示本次页面拿到 POST 响应的邀请；刷新后无法重建这份列表。</p>
          {sessionInvitations.map((invitation) => <article className="schedule-card" key={invitation.id}>
            <div className="schedule-card-heading"><div><h4>{invitation.invitee_id}</h4><p className="muted">{roleLabel(invitation.role)} · {invitationStateLabel(invitation)} · 邀请版本 v{invitation.version}</p></div></div>
            {invitation.state === 'pending' && <button type="button" className="button button-danger-quiet" disabled={!isOwner || Boolean(pendingAction)} onClick={() => revokeOutgoingInvitation(invitation)}>撤回邀请</button>}
          </article>)}
          {!sessionInvitations.length && <p className="muted">当前页面尚未创建邀请。</p>}
        </div>
      </section>

      <section className="connection-section" aria-labelledby="project-resources-title">
        <div className="connection-section-heading"><div><h2 id="project-resources-title">项目资源关联</h2><p className="muted">关联现有 Host 或 Workspace 不会复制资源，也不会授予项目成员额外权限。</p></div></div>
        {resourcesQuery.isPending ? <p role="status">正在读取资源关联…</p> : resourcesQuery.isError ? <p className="form-error" role="alert">读取资源关联失败：{errorDetails(resourcesQuery.error).message}</p> : <div className="schedule-list" aria-label="项目资源">
          {resources.map((resource) => <article className="schedule-card" key={`${resource.kind}:${resource.resource_id}`}>
            <div className="schedule-card-heading"><div><h3>{displayResource(resource)}</h3><p className="muted">关联版本 v{resource.version}</p></div></div>
            {resource.kind === 'workspace' && <button type="button" className="button button-secondary" onClick={() => setWorkspaceFileView({ projectID, workspaceID: resource.resource_id })}>查看文件目录</button>}
            {isOwner && <button type="button" className="button button-danger-quiet" disabled={Boolean(pendingAction) || !project} onClick={() => removeResource(resource)}>移除关联</button>}
          </article>)}
          {!resources.length && <p className="muted">没有当前账号可独立访问的项目资源关联。</p>}
        </div>}
        {visibleWorkspaceFileView && <div className="schedule-card" aria-label="Workspace 文件目录">
          <div className="schedule-card-heading"><div><h3>Workspace 文件目录</h3><p className="muted">仅显示已确认版本的路径、大小和权限位；不会读取文件内容。</p></div>
            <button type="button" className="button button-secondary" onClick={() => setWorkspaceFileView(null)}>关闭目录</button>
          </div>
          {workspaceManifestQuery.isPending ? <p role="status">正在读取 Workspace 文件目录…</p>
            : workspaceManifestQuery.isError ? <p className="form-error" role="alert">读取 Workspace 文件目录失败：{errorDetails(workspaceManifestQuery.error).message}</p>
              : workspaceManifestQuery.data && <>
                <p className="muted">Workspace {workspaceManifestQuery.data.workspace_id} · 已确认版本 v{workspaceManifestQuery.data.revision}</p>
                <ul aria-label="文件与空目录">
                  {workspaceManifestQuery.data.files.map((file) => <li key={`file:${file.path}`}><code>{file.path}</code> · {file.kind === 'symlink' ? '符号链接' : '文件'} · {file.size_bytes.toLocaleString('zh-CN')} B · {file.mode.toString(8)}</li>)}
                  {workspaceManifestQuery.data.empty_directories.map((path) => <li key={`dir:${path}`}><code>{path}/</code> · 空目录</li>)}
                </ul>
                {!workspaceManifestQuery.data.files.length && !workspaceManifestQuery.data.empty_directories.length && <p className="muted">此版本没有可显示的文件。</p>}
                {workspaceManifestQuery.data.next_cursor && <button type="button" className="button button-secondary" onClick={() => setWorkspaceFileView({
                  projectID, workspaceID: workspaceManifestQuery.data!.workspace_id,
                  revision: workspaceManifestQuery.data!.revision, cursor: workspaceManifestQuery.data!.next_cursor!,
                })}>下一页</button>}
              </>}
        </div>}
        <form onSubmit={(event) => { event.preventDefault(); addResource(); }} className="inline-form">
          <div><label htmlFor="resource-kind">资源类型</label><select id="resource-kind" value={resourceKind} onChange={(event) => setResourceKind(event.target.value as ResourceKind)}><option value="workspace">Workspace</option><option value="host">Host</option></select></div>
          <div><label htmlFor="resource-id">资源 ID</label><input id="resource-id" value={resourceID} onChange={(event) => setResourceID(event.target.value)} required /></div>
          <button type="submit" className="button button-secondary" disabled={!isOwner || !resourceID.trim() || Boolean(pendingAction) || !project}>{pendingAction.startsWith('add-resource:') ? '正在关联…' : '关联现有资源'}</button>
        </form>
      </section>

      <section className="connection-section" aria-labelledby="project-conversations-title">
        <div className="connection-section-heading"><div><h2 id="project-conversations-title">项目共享 AI 对话</h2><p className="muted">已接受的项目成员共享对话历史；对话创建者与每次 Task 的实际发起人分别记录。</p></div></div>
        {projectTaskMock
          ? <p className="notice notice-warning" role="note">本机 MSW 只展示预算归因并返回合成的 queued Task 回执；不会预留预算、调用模型或读取、写入文件。</p>
          : <p className="notice notice-info" role="note">真实项目 Task 由服务端检查并预留项目预算；未知用量按服务端账本状态处理，浏览器不会估算费用或绕过预算门禁。</p>}
        {!projectTaskMock && (projectTaskCapabilitiesQuery.isPending ? <p role="status">正在确认项目模型执行能力…</p>
            : projectTaskCapabilitiesQuery.isError ? <p className="notice notice-warning" role="alert">无法确认项目模型执行能力；真实项目 Task 提交已停用。</p>
              : !projectTaskExecutionReady && <p className="notice notice-warning" role="note">服务端尚未声明 `project_budget_execution`；真实项目 Task 提交已停用，须等服务端明确开放预算执行能力。</p>)}
        <form onSubmit={(event) => { event.preventDefault(); createProjectConversation(); }} className="inline-form">
          <div><label htmlFor="project-conversation-title-input">新共享对话标题（可选）</label><input id="project-conversation-title-input" value={projectConversationTitle} maxLength={120} onChange={(event) => setProjectConversationTitle(event.target.value)} /></div>
          <button type="submit" className="button button-secondary" disabled={!currentMember || Boolean(pendingAction || uncertainProjectAppend)}>{pendingAction === `create-project-conversation:${project.id}` ? '正在创建…' : '创建共享对话'}</button>
        </form>
        {!currentMember && !membersQuery.isPending && <p className="muted">只有已接受的项目成员可以创建共享对话或提交提示。</p>}
        {projectConversationsQuery.isPending ? <p role="status">正在读取共享对话…</p>
          : projectConversationsQuery.isError ? <p className="form-error" role="alert">读取项目共享对话失败：{errorDetails(projectConversationsQuery.error).message}</p>
            : projectConversations.length === 0 ? <p className="muted">项目还没有共享对话；创建后即可向所选模型提交提示。</p> : <>
              <div className="inline-form">
                <div><label htmlFor="project-conversation-select">共享对话</label><select id="project-conversation-select" value={selectedProjectConversation?.id ?? ''} disabled={Boolean(pendingAction || uncertainProjectAppend)} onChange={(event) => {
                  setSelectedProjectConversationID(event.target.value);
                  setProjectTaskReceipt(null);
                }}>
                  {projectConversations.map((conversation) => <option key={conversation.id} value={conversation.id}>{conversation.title}</option>)}
                </select></div>
                {selectedProjectConversation && <span className="muted">创建者 {selectedProjectConversation.owner_id} · 对话 {selectedProjectConversation.id}</span>}
              </div>
              <div className="schedule-list" aria-label="项目共享对话历史">
                <h3>共享历史</h3>
                {projectConversationMessagesQuery.isPending ? <p role="status">正在读取共享历史…</p>
                  : projectConversationMessagesQuery.isError ? <p className="form-error" role="alert">读取共享历史失败：{errorDetails(projectConversationMessagesQuery.error).message}</p>
                    : !projectConversationMessages.length ? <p className="muted">该共享对话暂无消息。</p>
                      : projectConversationMessages.map((message) => {
                        const task = message.task_id ? taskByID.get(message.task_id) : undefined;
                        const latestTask = visibleProjectTaskReceipt?.value.task;
                        const receiptStatus = latestTask && latestTask.id === message.task_id ? latestTask.status : undefined;
                        return <article className="schedule-card" key={message.id} aria-label="项目对话消息">
                          <div className="schedule-card-heading"><div><h4>{message.role === 'user' ? '成员提示' : 'AI 回复'}</h4><p className="muted">执行者 {message.actor_id} · 对话创建者 {message.owner_id}</p></div><span className="status">{message.delivery_status}</span></div>
                          <p>{message.content}</p>
                          {message.task_id && <p className="muted">Task {message.task_id} · 状态 {receiptStatus ?? task?.status ?? '未知'}</p>}
                        </article>;
                      })}
              </div>
              <form onSubmit={(event) => { event.preventDefault(); appendProjectPrompt(); }} className="form-stack">
                <label htmlFor="project-task-model">文本模型</label>
                <select id="project-task-model" value={effectiveProjectModelID} disabled={modelConnectionsQuery.isPending || !readyTextModels.length || Boolean(pendingAction || uncertainProjectAppend)} onChange={(event) => setProjectModelID(event.target.value)}>
                  {readyTextModels.map((model) => <option key={model.id} value={model.id}>{model.name} · {model.model_name}</option>)}
                </select>
                {modelConnectionsQuery.isPending ? <p role="status">正在读取可用模型…</p>
                  : modelConnectionsQuery.isError ? <p className="form-error" role="alert">读取模型失败：{errorDetails(modelConnectionsQuery.error).message}</p>
                    : !readyTextModels.length && <p className="muted">没有已验证的文本模型；请先在个人模型设置中配置可用模型。</p>}
                <div className="inline-form">
                  <div><label htmlFor="project-task-host">目标电脑（可选）</label><select id="project-task-host" value={selectedTaskHost?.id ?? ''} disabled={!resourcesQuery.isSuccess || taskHostsQuery.isPending || Boolean(pendingAction || uncertainProjectAppend)} onChange={(event) => {
                    setProjectHostID(event.target.value);
                    setProjectWorkspaceID('');
                  }}>
                    <option value="">云端 Task（不指定电脑）</option>
                    {taskHosts.map((host) => <option key={host.id} value={host.id}>{host.name} · {host.id}</option>)}
                  </select></div>
                  <div><label htmlFor="project-task-workspace">输入 Workspace 版本（可选）</label><select id="project-task-workspace" value={selectedTaskWorkspace?.id ?? ''} disabled={!resourcesQuery.isSuccess || taskWorkspacesQuery.isPending || !taskWorkspaces.length || Boolean(pendingAction || uncertainProjectAppend)} onChange={(event) => setProjectWorkspaceID(event.target.value)}>
                    <option value="">不指定 Workspace</option>
                    {taskWorkspaces.map((workspace) => <option key={workspace.id} value={workspace.id}>{workspace.name} · revision {workspace.revision}</option>)}
                  </select></div>
                </div>
                {resourcesQuery.isPending || taskHostsQuery.isPending || taskWorkspacesQuery.isPending
                  ? <p role="status">正在核实项目关联资源和当前账号的独立访问权限…</p>
                  : resourcesQuery.isError || taskHostsQuery.isError || taskWorkspacesQuery.isError
                    ? <p className="muted">部分目标资源状态暂不可用，因此未开放对应 Host/Workspace 选择；可提交不指定电脑或 Workspace 的 Task。</p>
                    : !taskHosts.length && !taskWorkspaces.length
                      ? <p className="muted">没有当前账号可独立访问且已关联到本项目的就绪 Host/Workspace；项目关联本身不会授予资源访问权。</p>
                      : selectedTaskHost && !taskWorkspaces.length && <p className="muted">可选 Workspace 必须属于本项目、当前账号可独立访问，并已在所选电脑应用到当前 revision。</p>}
                {invalidTaskTargetSelection && <p className="form-error" role="alert">所选目标已不可用或未与当前 revision 同步；请重新选择或清空目标。</p>}
                <label htmlFor="project-task-prompt">提交到共享对话的提示</label>
                <textarea id="project-task-prompt" value={projectPrompt} maxLength={32768} rows={4} disabled={Boolean(pendingAction || uncertainProjectAppend)} onChange={(event) => setProjectPrompt(event.target.value)} required />
                <div className="form-actions">
                  <button type="submit" className="button button-primary" disabled={!currentMember || !selectedProjectConversation || !effectiveProjectModelID || !projectPrompt.trim() || !projectTaskExecutionReady || invalidTaskTargetSelection || Boolean(pendingAction || uncertainProjectAppend)}>
                    {pendingAction.startsWith('append-project-prompt:') ? '正在提交…' : projectTaskExecutionReady ? '提交并排队项目 Task' : '项目 Task 执行未就绪'}
                  </button>
                  {uncertainProjectAppend && <button type="button" className="button button-secondary" disabled={Boolean(pendingAction)} onClick={retryProjectAppend}>
                    {pendingAction === uncertainProjectAppend.scope ? '正在核对同一请求…' : '重试同一项目 Task'}
                  </button>}
                </div>
              </form>
              {visibleProjectTaskReceipt && <article className="schedule-card" aria-label="项目 Task 回执">
                <div className="schedule-card-heading"><div><h3>项目 Task 回执</h3><p className="muted">{visibleProjectTaskReceipt.value.task.id} · 项目 {visibleProjectTaskReceipt.value.task.project_id} · 发起人 {visibleProjectTaskReceipt.value.task.initiator_id}</p></div><span className="status">{visibleProjectTaskReceipt.value.task.status}</span></div>
                <p>共享对话创建者 {visibleProjectTaskReceipt.value.message.owner_id} · 模型 {visibleProjectTaskReceipt.value.task.model_id}</p>
                <p>{visibleProjectTaskReceipt.value.message.content}</p>
                {visibleProjectTaskReceipt.value.task.host_id && <p>目标电脑：{visibleProjectTaskReceipt.value.task.host_id}</p>}
                {visibleProjectTaskReceipt.value.task.workspace_id && <p>输入 Workspace：{visibleProjectTaskReceipt.value.task.workspace_id} · revision {visibleProjectTaskReceipt.value.task.input_revision}</p>}
                <p className="muted">该状态来自服务端 Task 回执；排队不代表模型执行完成。</p>
              </article>}
            </>}
      </section>

      <section className="connection-section" aria-labelledby="project-budget-title">
        <div className="connection-section-heading"><div><h2 id="project-budget-title">项目预算与账本</h2><p className="muted">金额使用整数微单位；未知费用按接口状态显示，不在浏览器估算或清零。</p></div></div>
        {budgetQuery.isPending ? <p role="status">正在读取预算…</p> : budgetQuery.isError ? <p className="form-error" role="alert">读取预算失败：{errorDetails(budgetQuery.error).message}</p> : budget && <>
          <div className="notice notice-info" role="status">
            {budget.configured ? <>额度 {safeMoney(budget.limit_micros, budget.currency)} · 已结算 {safeMoney(budget.spent_micros, budget.currency)} · 已预留 {safeMoney(budget.reserved_micros, budget.currency)} · 预算版本 v{budget.version}</> : '尚未配置项目预算。'}
            {budget.has_unknown && <strong> 存在未知费用，需等待服务端对账。</strong>}
          </div>
          <p className="muted">{projectTaskMock
            ? '本机 MSW 只展示合成预算归因；保存限额不会预留真实模型费用或启动 Task。'
            : '服务端会在模型调用前预留项目预算；此处仅展示预算与账本回执，不启动 Task，未知费用由服务端对账。'}</p>
          <form onSubmit={(event) => { event.preventDefault(); setProjectBudget(); }} className="inline-form">
            <div><label htmlFor="budget-currency">币种（3 位大写代码）</label><input id="budget-currency" value={budgetCurrency} maxLength={3} onChange={(event) => setBudgetCurrency(event.target.value.toUpperCase())} pattern="[A-Z]{3}" required /></div>
            <div><label htmlFor="budget-limit">预算限额（主币种单位，最多 6 位小数）</label><input id="budget-limit" inputMode="decimal" value={budgetLimit} onChange={(event) => setBudgetLimit(event.target.value)} placeholder="10.00" required /></div>
            <button type="submit" className="button button-secondary" disabled={!isOwner || !canSubmitBudget || Boolean(pendingAction)}>{pendingAction === `set-budget:${project.id}` ? '正在保存…' : '保存预算'}</button>
          </form>
          {!isOwner && <p className="muted">只有负责人可以修改预算。</p>}
        </>}
        <h3>服务端账本</h3>
        {ledgerQuery.isPending ? <p role="status">正在读取账本…</p> : ledgerQuery.isError ? <p className="form-error" role="alert">读取账本失败：{errorDetails(ledgerQuery.error).message}</p> : !ledger.length ? <p className="muted">服务端尚无账本条目。</p> : <div className="schedule-list">
          {ledger.map((entry) => <article className="schedule-card" key={entry.call_id}>
            <div className="schedule-card-heading"><div><h4>{entry.source} · {entry.model_id}</h4><p className="muted">调用 {entry.call_id} · 执行者 {entry.actor_id} · 费率版本 v{entry.rate_version}</p></div><span className={`status ${entry.status === 'settled' ? 'status-running' : entry.status === 'unknown' ? 'status-failed' : ''}`}>{entry.status}</span></div>
            <p>预留 {safeMoney(entry.reserved_micros, entry.currency)} · 实际 {entry.actual_micros === undefined ? (entry.status === 'unknown' ? '未知' : '未结算') : safeMoney(entry.actual_micros, entry.currency)}</p>
            <p className="muted">预留用量：输入 {entry.reserved_usage.input_tokens.toLocaleString('zh-CN')} / 输出 {entry.reserved_usage.output_tokens.toLocaleString('zh-CN')} tokens · {usageLabel(entry)}</p>
            {entry.task_id && <p className="muted">Task：{entry.task_id}</p>}
          </article>)}
        </div>}
      </section>

      <section className="connection-section" aria-labelledby="project-work-title">
        <div className="connection-section-heading"><div><h2 id="project-work-title">项目 Todo 与工作项</h2><p className="muted">只显示服务端状态。创建工作项不启动执行；Task 必须先存在并通过服务端授权后再关联。</p></div></div>
        <form onSubmit={(event) => { event.preventDefault(); createWorkItem(); }} className="form-stack">
          <label htmlFor="work-title">工作项标题</label><input id="work-title" value={workTitle} maxLength={200} onChange={(event) => setWorkTitle(event.target.value)} required />
          <label htmlFor="work-description">说明</label><textarea id="work-description" value={workDescription} maxLength={4096} rows={3} onChange={(event) => setWorkDescription(event.target.value)} />
          <label htmlFor="work-criteria">验收标准（每行一项）</label><textarea id="work-criteria" value={acceptanceCriteria} rows={3} onChange={(event) => setAcceptanceCriteria(event.target.value)} />
          <div className="form-actions"><button type="submit" className="button button-primary" disabled={!canManageWork || !workTitle.trim() || Boolean(pendingAction)}>{pendingAction === `create-work-item:${project.id}` ? '正在创建…' : '创建工作项'}</button></div>
        </form>
        {!canEditWork && <p className="muted">当前成员角色不能修改工作项；等待角色数据或使用负责人/成员账号。</p>}

        {workItemsQuery.isPending ? <p role="status">正在读取工作项…</p> : workItemsQuery.isError ? <p className="form-error" role="alert">读取工作项失败：{errorDetails(workItemsQuery.error).message}</p> : <div className="schedule-list" aria-label="项目工作项">
          {workItems.map((item) => {
            const linkedTaskIDs = new Set(item.task_ids);
            const taskCandidates = tasks.filter((task) => !linkedTaskIDs.has(task.id));
            const deliveryChoice = deliveryChoices[item.id] ?? { taskID: item.task_ids[0] ?? '', workspaceID: '', revision: '' };
            return <article className="schedule-card" key={item.id}>
              <div className="schedule-card-heading"><div><h3>{item.title}</h3><p className="muted">{item.id} · 工作项版本 v{item.version} · 负责人 {item.assignee_id}</p></div><span className={`status ${item.state === 'open' ? 'status-running' : item.state === 'delivered' ? 'status-completed' : ''}`}>{workItemStateLabel(item)}</span></div>
              {item.description && <p>{item.description}</p>}
              {item.acceptance_criteria.length > 0 && <div><strong>验收标准</strong><ul>{item.acceptance_criteria.map((criterion, index) => <li key={`${index}:${criterion}`}>{criterion}</li>)}</ul></div>}
              {item.blocked_reason && <p className="notice notice-warning">阻塞原因：{item.blocked_reason}</p>}
              <div className="schedule-actions">
                <label htmlFor={`assignee-${item.id}`}>负责人</label>
                <select id={`assignee-${item.id}`} value={item.assignee_id} disabled={!canManageWork || Boolean(pendingAction)} onChange={(event) => assignWorkItem(item, event.target.value)}>
                  {members.map((member) => <option key={member.user_id} value={member.user_id}>{member.user_id}</option>)}
                </select>
              </div>
              {item.state === 'open' && <div className="inline-form">
                <div><label htmlFor={`block-reason-${item.id}`}>阻塞原因</label><input id={`block-reason-${item.id}`} maxLength={2000} value={blockReasons[item.id] ?? ''} onChange={(event) => setBlockReasons((current) => ({ ...current, [item.id]: event.target.value }))} /></div>
                <button type="button" className="button button-secondary" disabled={!canManageWork || !blockReasons[item.id]?.trim() || Boolean(pendingAction)} onClick={() => blockWorkItem(item)}>阻塞工作项</button>
              </div>}
              {item.state === 'blocked' && <button type="button" className="button button-secondary" disabled={!canManageWork || Boolean(pendingAction)} onClick={() => resumeWorkItem(item)}>恢复工作项</button>}

              <div className="workspace-host-status">
                <strong>关联 Task</strong>
                {item.task_ids.length ? item.task_ids.map((taskID) => {
                  const task = taskByID.get(taskID);
                  return <small key={taskID}>{taskID}{task ? ` · ${task.status} · ${task.objective}` : ' · 当前账号的 Task 列表中不可见'}</small>;
                }) : <small>没有已关联 Task。</small>}
                {tasksQuery.isError && <small role="alert">读取 Task 列表失败：{errorDetails(tasksQuery.error).message}</small>}
              </div>
              <div className="inline-form">
                <div><label htmlFor={`task-link-${item.id}`}>关联已有 Task</label><select id={`task-link-${item.id}`} value={taskChoices[item.id] ?? ''} disabled={!canManageWork || tasksQuery.isPending || Boolean(pendingAction)} onChange={(event) => setTaskChoices((current) => ({ ...current, [item.id]: event.target.value }))}>
                  <option value="">选择自己的 Task</option>{taskCandidates.map((task) => <option key={task.id} value={task.id}>{task.id} · {task.status} · {task.objective}</option>)}
                </select></div>
                <button type="button" className="button button-secondary" disabled={!canManageWork || !taskChoices[item.id] || Boolean(pendingAction)} onClick={() => linkTask(item)}>关联 Task</button>
              </div>
              <p className="muted">当前合同只提供关联已有 Task 的接口，没有“从工作项新建并启动 Task”的专用操作。</p>

              {item.state !== 'delivered' && <div className="form-stack">
                <h4>提交交付证据</h4>
                <label htmlFor={`delivery-task-${item.id}`}>已关联 Task</label>
                <select id={`delivery-task-${item.id}`} value={deliveryChoice.taskID} disabled={!canManageWork || Boolean(pendingAction)} onChange={(event) => setDeliveryChoices((current) => ({ ...current, [item.id]: { ...deliveryChoice, taskID: event.target.value } }))}>
                  <option value="">先关联 Task</option>{item.task_ids.map((taskID) => <option key={taskID} value={taskID}>{taskID}</option>)}
                </select>
                <label htmlFor={`delivery-workspace-${item.id}`}>已保存 Workspace ID</label>
                <input id={`delivery-workspace-${item.id}`} value={deliveryChoice.workspaceID} onChange={(event) => setDeliveryChoices((current) => ({ ...current, [item.id]: { ...deliveryChoice, workspaceID: event.target.value } }))} />
                <label htmlFor={`delivery-revision-${item.id}`}>已保存版本号</label>
                <input id={`delivery-revision-${item.id}`} type="number" min="1" step="1" value={deliveryChoice.revision} onChange={(event) => setDeliveryChoices((current) => ({ ...current, [item.id]: { ...deliveryChoice, revision: event.target.value } }))} />
                <p className="muted">服务端会验证 Task 关联、资源范围和版本是否已持久化；验证不可用时 API 可返回 501，页面会保留错误，不会标记为已交付。</p>
                <div className="form-actions"><button type="button" className="button button-secondary" disabled={!canManageWork || !deliveryChoice.taskID || !deliveryChoice.workspaceID.trim() || !Number.isSafeInteger(Number(deliveryChoice.revision)) || Number(deliveryChoice.revision) < 1 || Boolean(pendingAction)} onClick={() => deliverWorkItem(item)}>提交交付证据</button></div>
              </div>}
              {item.delivery && <p className="notice notice-success">服务端交付证据：Task {item.delivery.task_id} · Workspace {item.delivery.workspace_id} · revision {item.delivery.revision}</p>}
            </article>;
          })}
          {!workItems.length && <div className="empty-card compact"><p>服务端尚无工作项。</p></div>}
        </div>}
      </section>
    </>}
    <ProjectMuseExtensions />
  </section>;
}
