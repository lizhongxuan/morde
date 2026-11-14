import { useLayoutEffect, useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiError } from '../api/client';
import type { components } from '../api/schema';
import { errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import { useAuth } from '../app/auth';
import { useMessageOutbox } from '../app/use-message-outbox';
import { useMessageVariantOutbox } from '../app/use-message-variant-outbox';
import type { MessageVariantOutboxEntry } from '../app/use-message-variant-outbox';
import { buildMessageVariantTimeline, createMessageVariant, maxMessageVariantScalars, messageVariantActionLabel, messageVariantContentTooLong } from '../app/message-variants';
import type { MessageVariantAction, VersionableMessage } from '../app/message-variants';
import { needsMessageUpload, uploadMessage } from '../app/message-upload';
import { MessageReceiptOrder } from '../app/message-receipt-order';
import { deliveryStatusText, routingFailureText, routingStatusText } from '../app/message-status';
import { clearProcessingRetryKey, processingRetryKey } from '../app/message-processing-retry';
import type { MessageOutboxEntry } from '../app/use-message-outbox';
import type { PendingConversationMessage } from '../app/message-outbox';
import { outboxReceiptRequest } from '../app/message-outbox';
import { ChatMarkdown } from '../components/ChatMarkdown';
import { ScreenPagination } from '../components/ScreenPagination';
import { AssistantReplySpeech } from '../components/AssistantReplySpeech';
import { AssistantVoiceInput } from '../components/AssistantVoiceInput';
import { projectApi } from '../app/project-api';
import type { CreateProjectWorkItemRequest, Project, ProjectConversation, ProjectConversationMessage, ProjectInvitation, ProjectMember, ProjectResource, ProjectRole, ProjectWorkItem, ProjectWorkItemCategory } from '../app/project-api';
import { FloatingError, FloatingNotice } from '../components/FloatingError';
import './MessageVariants.css';

type RequestKeyRecord = { fingerprint: string; key: string };
type ActionNotice = { kind: 'success' | 'error' | 'unknown'; message: string };
type LedgerEntry = components['schemas']['ProjectBudgetLedgerEntry'];
type ResourceKind = components['schemas']['ProjectResourceKind'];
type ProjectMessageReceipt = { projectID: string; conversationID: string };
type ProjectTask = Awaited<ReturnType<typeof projectApi.tasks>>[number];
type ProjectVersionableMessage = ProjectConversationMessage & Pick<VersionableMessage, 'variant_group_id' | 'variant_version' | 'variant_action'>;

function definiteProjectMessageAcceptanceFailure(error: unknown) {
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

function workItemCategoryLabel(category: ProjectWorkItemCategory | undefined) {
  return ({ bug: '缺陷', request: '需求', unclassified: '未分类' })[category ?? 'unclassified'];
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
  const { user } = useAuth();
  if (user?.role !== 'user') return <section className="connection-section" role="status">
    <h2>项目功能仅支持普通用户</h2>
    <p className="muted">当前管理员账号没有项目成员身份。请使用普通用户账号打开项目，管理员账号仍可使用其他用户端功能。</p>
  </section>;
  return <UserProjects />;
}

function UserProjects() {
  const queryClient = useQueryClient();
  const { user } = useAuth();
  const [selectedProjectID, setSelectedProjectID] = useState('');
  const [showProjectDetails, setShowProjectDetails] = useState(false);
  const [projectName, setProjectName] = useState('');
  const [projectDescription, setProjectDescription] = useState('');
  const [showCreateProject, setShowCreateProject] = useState(false);
  const [inviteUserID, setInviteUserID] = useState('');
  const [inviteRole, setInviteRole] = useState<Exclude<ProjectRole, 'owner'>>('member');
  const [ownerTargetID, setOwnerTargetID] = useState('');
  const [resourceKind, setResourceKind] = useState<ResourceKind>('workspace');
  const [resourceID, setResourceID] = useState('');
  const [workspaceFileView, setWorkspaceFileView] = useState<{ projectID: string; workspaceID: string; revision?: number; cursor?: string } | null>(null);
  const [workTitle, setWorkTitle] = useState('');
  const [workDescription, setWorkDescription] = useState('');
  const [acceptanceCriteria, setAcceptanceCriteria] = useState('');
  const [workCategory, setWorkCategory] = useState<ProjectWorkItemCategory>('bug');
  const [workItemFilter, setWorkItemFilter] = useState<'all' | 'bug' | 'request' | 'unclaimed'>('all');
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
  const [projectMessageReceipt, setProjectMessageReceipt] = useState<ProjectMessageReceipt | null>(null);
  const [selectedMessageVariantVersions, setSelectedMessageVariantVersions] = useState<Record<string, number>>({});
  const [editingMessageVariant, setEditingMessageVariant] = useState<{ groupID: string; messageID: string; content: string } | null>(null);
  const pendingProjectSave = useRef<Promise<void>>(Promise.resolve());
  const sendingProjectMessages = useRef(new Set<string>());
  const sendingVariantOperationKeys = useRef(new Set<string>());
  const projectReceiptOrder = useRef(new MessageReceiptOrder());
  const [localOutgoingInvitations, setLocalOutgoingInvitations] = useState<Record<string, ProjectInvitation[]>>({});
  const [pendingAction, setPendingAction] = useState('');
  const [notice, setNotice] = useState<ActionNotice | null>(null);
  const [retryAcceptedMessageIDs, setRetryAcceptedMessageIDs] = useState<Set<string>>(() => new Set());
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
  const projectConversationsQuery = useQuery({ queryKey: ['project-conversations', projectID], queryFn: () => projectApi.projectConversations(projectID), enabled: Boolean(projectID), retry: false });
  const modelConnectionsQuery = useQuery({ queryKey: ['model-connections'], queryFn: portalApi.modelConnections, enabled: Boolean(projectID), retry: false });
  const taskHostsQuery = useQuery({ queryKey: ['project-task-hosts'], queryFn: portalApi.hosts, enabled: Boolean(projectID), retry: false });
  const taskWorkspacesQuery = useQuery({ queryKey: ['project-task-workspaces'], queryFn: portalApi.workspaces, enabled: Boolean(projectID), retry: false });
  const projectTaskCapabilitiesQuery = useQuery({ queryKey: ['service-capabilities'], queryFn: portalApi.capabilities, enabled: Boolean(projectID), retry: false });
  const outbox = useMessageOutbox(user?.id, projectTaskCapabilitiesQuery.data?.service_instance_id);
  const variantOutbox = useMessageVariantOutbox(user?.id, projectTaskCapabilitiesQuery.data?.service_instance_id);
  const activeOutboxScope = useRef(outbox.currentScope);
  useLayoutEffect(() => { activeOutboxScope.current = outbox.currentScope; }, [outbox.currentScope]);
  const projectTaskExecutionReady = projectTaskMock || projectTaskCapabilitiesQuery.data?.supported_features.includes('project_budget_execution') === true;
  const projectWorkItemsV2 = projectTaskCapabilitiesQuery.data?.supported_features.includes('project_work_items_v2') === true;
  const workItemsQuery = useQuery({
    queryKey: ['project-work-items', projectID, projectWorkItemsV2],
    queryFn: () => projectApi.workItems(projectID, projectWorkItemsV2),
    enabled: Boolean(projectID), retry: false,
  });
  const workItems = workItemsQuery.data ?? [];
  const visibleWorkItems = workItems.filter((item) => {
    if (!projectWorkItemsV2 || workItemFilter === 'all') return true;
    if (workItemFilter === 'unclaimed') return item.assignee_id === null;
    return item.category === workItemFilter;
  });
  const budgetQuery = useQuery({ queryKey: ['project-budget', projectID], queryFn: () => projectApi.budget(projectID), enabled: Boolean(projectID), retry: false });
  const budget = budgetQuery.data;
  const ledgerQuery = useQuery({ queryKey: ['project-budget-ledger', projectID], queryFn: () => projectApi.ledger(projectID), enabled: Boolean(projectID), retry: false });
  const ledger = ledgerQuery.data ?? [];
  const invitationsQuery = useQuery({ queryKey: ['my-project-invitations'], queryFn: projectApi.myInvitations, retry: false });
  const invitations = invitationsQuery.data ?? [];
  const tasksQuery = useQuery({ queryKey: ['project-task-candidates'], queryFn: projectApi.tasks, enabled: Boolean(projectID), retry: false,
    refetchInterval: (query) => query.state.data?.some((task) => ['queued', 'running', 'waiting_user', 'pausing', 'paused', 'cancelling'].includes(task.status)) ? 3000 : false });
  const tasks = tasksQuery.data ?? [];
  const projectConversations = projectConversationsQuery.data ?? [];
  const selectedProjectConversation = projectConversations.find((item) => item.id === selectedProjectConversationID)
    ?? projectConversations[0];
  const projectConversationMessagesQuery = useQuery({
    queryKey: ['project-conversation-messages', projectID, selectedProjectConversation?.id],
    queryFn: () => projectApi.projectConversationMessages(projectID, selectedProjectConversation!.id),
    enabled: Boolean(projectID && selectedProjectConversation), retry: false,
    refetchInterval: (query) => query.state.data?.some((message) => message.output_status === 'streaming') ? 1000
      : query.state.data?.some((message) => message.routing_status === 'pending' || message.routing_status === 'processing' || message.routing_status === 'waiting_adjustment') ? 2000 : 5000,
  });
  const projectMessagesByID = new Map<string, ProjectConversationMessage>();
  for (const message of projectConversationMessagesQuery.data ?? []) {
    if (!projectMessagesByID.has(message.id)) projectMessagesByID.set(message.id, message);
  }
  const projectConversationMessages = [...projectMessagesByID.values()].sort((left, right) =>
    left.sequence - right.sequence || left.created_at.localeCompare(right.created_at) || left.id.localeCompare(right.id));
  const selectedVariantMap = new Map(Object.entries(selectedMessageVariantVersions).map(([groupID, version]) => [groupID, version]));
  const projectVariantTimeline = buildMessageVariantTimeline(projectConversationMessages as ProjectVersionableMessage[], selectedVariantMap);
  const visibleProjectConversationMessages = projectVariantTimeline.items;
  const readyTextModels = modelConnectionsQuery.data?.items.filter((model) => model.checked_at && model.capabilities.text) ?? [];
  const effectiveProjectModelID = projectModelID || (readyTextModels.length === 1 ? readyTextModels[0]?.id : '') || '';

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
  const visibleProjectMessageReceipt = projectMessageReceipt?.projectID === projectID
    && projectMessageReceipt.conversationID === selectedProjectConversation?.id ? projectMessageReceipt : null;
  const pendingProjectMessages = outbox.entries.filter(({ message }) => message.projectID === projectID
    && message.conversationID === selectedProjectConversation?.id);

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
        setProjectMessageReceipt(null);
      },
      successMessage: '服务端已创建项目共享对话；尚未启动 Task。',
    });
  };

  const sendProjectMessage = async (entry: MessageOutboxEntry, reconcile: boolean) => {
    let { scope, message } = entry;
    const id = message.request.client_message_id;
    if (sendingProjectMessages.current.has(id)) return;
    sendingProjectMessages.current.add(id);
    const finishReceipt = projectReceiptOrder.current.begin(scope, id);
    try {
      if (scope === outbox.unknownScope) {
        if (!projectTaskCapabilitiesQuery.data?.service_instance_id || !reconcile) {
          await outbox.change(scope, id, (current) => ({ ...current, status: 'pending', error: '等待确认原服务和账号。' }));
          return;
        }
        message = { ...message, serviceInstanceID: projectTaskCapabilitiesQuery.data.service_instance_id };
        const nextScope = await outbox.add(message);
        await outbox.remove(scope, id);
        scope = nextScope;
      }
      if (scope !== activeOutboxScope.current || !message.projectID || !message.conversationID || !user || message.actorID !== user.id || !message.serviceInstanceID
        || message.serviceInstanceID !== projectTaskCapabilitiesQuery.data?.service_instance_id) {
        throw new Error('账号或服务尚未确认，消息保留在本机。');
      }
      const currentCapabilities = await portalApi.capabilities();
      if (currentCapabilities.service_instance_id !== message.serviceInstanceID) {
        throw new Error('服务身份已变化，消息保留在原服务的本机队列中。');
      }
      const targetProjectID = message.projectID;
      const targetConversationID = message.conversationID;
      const checkScope = () => {
        if (scope !== activeOutboxScope.current) throw new Error('账号或服务已变化，消息保留在本机。');
      };
      await outbox.change(scope, id, (current) => ({ ...current, status: 'sending', error: undefined }));
      checkScope();
      let receipt = reconcile ? (await projectApi.projectConversationMessages(targetProjectID, targetConversationID, id, message.serviceInstanceID, message.actorID))
        .find((item) => item.client_message_id === id) : undefined;
      if (!receipt) {
        checkScope();
        const request = outboxReceiptRequest(message);
        if (request.client_attachment_ids?.length) throw new Error('项目附件上传尚未在此页面开放；原消息和附件已保留。');
        const previousID = request.previous_client_message_id;
        if (previousID) {
          if (previousID === id) throw new Error('消息前序编号无效，原消息已保留。');
          const pending = projectReceiptOrder.current.previous(scope, previousID);
          const previousAccepted = pending ? await pending : (await projectApi.projectConversationMessages(
            targetProjectID, targetConversationID, previousID, message.serviceInstanceID, message.actorID,
          )).some((item) => item.conversation_id === targetConversationID && item.owner_id === message.conversationOwnerID &&
            item.actor_id === message.actorID && item.role === 'user' && item.client_message_id === previousID);
          checkScope();
          if (!previousAccepted) {
            await outbox.change(scope, id, (current) => ({ ...current, status: 'pending', error: '前一条消息尚未确认，请先核对后重试。' }));
            return;
          }
        }
        receipt = needsMessageUpload(request.content)
          ? await uploadMessage<components['schemas']['ProjectConversationMessage']>(
            { projectID: targetProjectID, conversationID: targetConversationID },
            request, message.serviceInstanceID, message.actorID, checkScope,
          )
          : (await projectApi.acceptProjectConversationMessage(targetProjectID, targetConversationID, request, id, message.serviceInstanceID, message.actorID)).message;
      }
      checkScope();
      if (receipt.conversation_id !== targetConversationID || receipt.actor_id !== user.id || receipt.owner_id !== message.conversationOwnerID
        || receipt.client_message_id !== id || receipt.content !== message.request.content || receipt.role !== 'user') {
        throw new Error('服务端消息回执与项目或发起人不一致，请核对后重试。');
      }
      finishReceipt(true);
      queryClient.setQueryData<components['schemas']['ProjectConversationMessage'][]>(
        ['project-conversation-messages', targetProjectID, targetConversationID], (previous) => previous?.some((item) => item.id === receipt!.id)
          ? previous : [...(previous ?? []), receipt!],
      );
      setProjectMessageReceipt({ projectID: targetProjectID, conversationID: targetConversationID });
      await outbox.remove(scope, id);
      void queryClient.invalidateQueries({ queryKey: ['project-conversation-messages', targetProjectID, targetConversationID] });
      void queryClient.invalidateQueries({ queryKey: ['project-task-candidates'] });
    } catch (error) {
      const definite = definiteProjectMessageAcceptanceFailure(error);
      const uploadWaiting = error instanceof ApiError && error.code === 'message_upload_unavailable';
      const details = error instanceof ApiError && error.status === 409
        ? '账号、服务或消息状态已变化，消息保留在本机，请核对后重试。' : errorDetails(error).message;
      await outbox.change(scope, id, (current) => ({ ...current, status: uploadWaiting ? 'pending' : definite ? 'failed' : 'unknown', error: details }))
        .catch(() => setNotice({ kind: 'error', message: '无法更新本地消息状态，消息将保留等待核对。' }));
    } finally {
      finishReceipt(false);
      sendingProjectMessages.current.delete(id);
    }
  };
  const retryProjectProcessing = useMutation({
    mutationFn: async (target: { projectID: string; conversationID: string; messageID: string }) => {
      if (!user?.id || !projectTaskCapabilitiesQuery.data?.service_instance_id) throw new Error('账号或服务尚未确认。');
      const instanceID = projectTaskCapabilitiesQuery.data.service_instance_id;
      const current = await portalApi.capabilities();
      if (current.service_instance_id !== instanceID || !current.supported_features.includes('conversation_auto_routing_v1')) {
        throw new Error('服务身份已变化或尚不支持消息处理，请核对后重试。');
      }
      const key = processingRetryKey(instanceID, user.id, target.projectID, target.conversationID, target.messageID);
      await projectApi.retryProjectConversationMessageProcessing(target.projectID, target.conversationID, target.messageID, key, instanceID, user.id);
      clearProcessingRetryKey(instanceID, user.id, target.projectID, target.conversationID, target.messageID);
    },
    onSuccess: async (_value, target) => {
      setRetryAcceptedMessageIDs((current) => new Set(current).add(target.messageID));
      window.setTimeout(() => setRetryAcceptedMessageIDs((current) => {
        if (!current.has(target.messageID)) return current;
        const next = new Set(current);
        next.delete(target.messageID);
        return next;
      }), 30_000);
      await queryClient.invalidateQueries({ queryKey: ['project-conversation-messages', target.projectID, target.conversationID] });
    },
    onError: (error, target) => {
      setRetryAcceptedMessageIDs((current) => {
        if (!current.has(target.messageID)) return current;
        const next = new Set(current);
        next.delete(target.messageID);
        return next;
      });
      setNotice({ kind: 'error', message: errorDetails(error).message });
    },
  });

  const sendProjectVariantOperation = async (entry: MessageVariantOutboxEntry): Promise<boolean> => {
    const key = entry.operation.input.idempotencyKey;
    if (sendingVariantOperationKeys.current.has(key)) return false;
    sendingVariantOperationKeys.current.add(key);
    try {
      if (entry.scope !== variantOutbox.currentScope || entry.operation.input.actorID !== user?.id ||
        entry.operation.input.serviceInstanceID !== projectTaskCapabilitiesQuery.data?.service_instance_id) {
        throw new Error('账号或服务身份已变化；此操作保留在本机，请核对后重新发起。');
      }
      await variantOutbox.change(entry.scope, key, (operation) => ({ ...operation, status: 'sending', error: undefined }));
      const currentCapabilities = await portalApi.capabilities();
      if (currentCapabilities.service_instance_id !== entry.operation.input.serviceInstanceID) {
        throw new Error('服务身份已变化；此操作未重放，请核对后重新发起。');
      }
      const result = await createMessageVariant(entry.operation.input);
      if (result.state === 'unknown') {
        await variantOutbox.change(entry.scope, key, (operation) => ({ ...operation, status: 'unknown', error: '服务端尚未确认操作结果；重试会复用原操作编号。' }));
        void queryClient.invalidateQueries({ queryKey: ['project-conversation-messages', entry.operation.input.projectID, entry.operation.input.conversationID] });
        setNotice({ kind: 'unknown', message: '操作结果待核对；可以使用同一请求编号安全重试。' });
        return false;
      }
      await variantOutbox.remove(entry.scope, key);
      if (result.state !== 'failed') {
        setSelectedMessageVariantVersions((current) => ({ ...current, [result.variant_group_id]: result.variant_version }));
        setEditingMessageVariant((current) => current?.groupID === result.variant_group_id ? null : current);
      }
      void queryClient.invalidateQueries({ queryKey: ['project-conversation-messages', entry.operation.input.projectID, entry.operation.input.conversationID] });
      void queryClient.invalidateQueries({ queryKey: ['project-task-candidates'] });
      if (result.state === 'failed') {
        setNotice({ kind: 'error', message: '服务端已确认项目消息版本操作失败；共享历史已刷新。' });
        return false;
      }
      setNotice({ kind: 'success', message: result.state === 'completed' ? '已创建新的项目消息版本。' : '项目消息版本操作已受理，正在更新共享历史。' });
      return true;
    } catch (error) {
      const staleVersion = error instanceof ApiError && error.status === 409;
      const uncertain = !(error instanceof ApiError) || error.status >= 500 || [408, 425, 429].includes(error.status);
      const details = errorDetails(error);
      await variantOutbox.change(entry.scope, key, (operation) => ({ ...operation,
        status: staleVersion ? 'failed' : uncertain ? 'unknown' : 'failed', error: staleVersion
          ? '版本已变化，已刷新共享历史；请选中最新版本后重新操作。' : details.message }));
      if (staleVersion) {
        await queryClient.invalidateQueries({ queryKey: ['project-conversation-messages', entry.operation.input.projectID, entry.operation.input.conversationID] });
        setNotice({ kind: 'error', message: '项目消息版本已变化，已刷新共享历史；请核对后再操作。' });
      } else setNotice({ kind: uncertain ? 'unknown' : 'error', message: uncertain
        ? '操作结果待核对；可以使用同一请求编号安全重试。' : details.message });
      return false;
    } finally {
      sendingVariantOperationKeys.current.delete(key);
    }
  };

  const startProjectVariantOperation = async (groupID: string, messageID: string, action: MessageVariantAction, contentValue?: string) => {
    const group = projectVariantTimeline.groups.get(groupID);
    const serviceInstanceID = projectTaskCapabilitiesQuery.data?.service_instance_id;
    if (!project || !selectedProjectConversation || !group || !user?.id || !serviceInstanceID || !currentMember || currentMember.role === 'viewer') {
      setNotice({ kind: 'error', message: '项目成员或服务身份尚未确认，无法执行此操作。' });
      return false;
    }
    try {
      const operation = await variantOutbox.enqueue({
        projectID: project.id, conversationID: selectedProjectConversation.id, messageID, action,
        expectedVersion: group.latestVersion, ...(contentValue !== undefined ? { content: contentValue } : {}),
        serviceInstanceID, actorID: user.id,
      });
      return sendProjectVariantOperation(operation);
    } catch (error) {
      setNotice({ kind: 'error', message: errorDetails(error).message });
      return false;
    }
  };

  const stopProjectTask = (task: ProjectTask) => {
    if (!project || task.initiator_id !== user?.id || task.conversation_id !== selectedProjectConversation?.id) return;
    const hasCurrentRevision = task.current_revision !== undefined;
    const hasDesiredRevision = task.desired_revision !== undefined;
    if (hasCurrentRevision !== hasDesiredRevision ||
      (hasCurrentRevision && (!Number.isSafeInteger(task.current_revision) || task.current_revision! < 0 ||
        !Number.isSafeInteger(task.desired_revision) || task.desired_revision! < 0))) {
      setNotice({ kind: 'error', message: '任务修订信息不完整或无效，请刷新状态后重试。' });
      return;
    }
    const body: components['schemas']['TaskActionRequest'] = {
      action: 'cancel', expected_status: task.status,
      ...(hasCurrentRevision && hasDesiredRevision ? {
        expected_current_revision: task.current_revision!, expected_desired_revision: task.desired_revision!,
      } : {}),
    };
    const scope = `stop-project-task:${project.id}:${task.id}`;
    const key = requestKeyFor(scope, body);
    void runAction({
      action: scope, run: () => portalApi.taskAction(task.id, body, key), requestKey: { scope, key },
      successMessage: '停止请求已受理，正在等待服务端确认任务状态。',
    });
  };

  const queueProjectMessage = () => {
    const content = projectPrompt;
    if (!project || !selectedProjectConversation || !content.trim()) return;
    // Freshness is evaluated when the user submits the form.
    const now = Date.now();
    if (modelConnectionsQuery.isSuccess && now - modelConnectionsQuery.dataUpdatedAt < 60_000
      && modelConnectionsQuery.data.configuration_status === 'not_configured') {
      setNotice({ kind: 'error', message: '请先配置云端模型，输入内容已保留。' });
      return;
    }
    const id = newIdempotencyKey();
    const targetProjectID = project.id;
    const targetConversation = selectedProjectConversation;
    const serverPredecessor = [...projectConversationMessages].reverse()
      .find((item) => item.role === 'user' && item.actor_id === user?.id && item.client_message_id)?.client_message_id;
    const previous = pendingProjectSave.current.then(async () => {
      const message: PendingConversationMessage = {
        actorID: user?.id ?? '',
        request: {
          client_message_id: id, content,
          time_zone: Intl.DateTimeFormat().resolvedOptions().timeZone,
          ...(effectiveProjectModelID ? { model_id: effectiveProjectModelID } : {}),
          ...(projectHostID ? { host_id: projectHostID } : {}),
          ...(projectWorkspaceID ? { workspace_id: projectWorkspaceID } : {}),
        },
        projectID: targetProjectID, conversationID: targetConversation.id, conversationOwnerID: targetConversation.owner_id,
        ...(projectTaskCapabilitiesQuery.data?.service_instance_id ? { serviceInstanceID: projectTaskCapabilitiesQuery.data.service_instance_id } : {}),
        attachments: [], status: 'pending', createdAt: Date.now(),
      };
      const entry = await outbox.enqueue(message, serverPredecessor);
      if (activeOutboxScope.current === entry.scope) setProjectPrompt((current) => current === content ? '' : current);
      void sendProjectMessage(entry, false);
    });
    pendingProjectSave.current = previous.catch((error) => setNotice({ kind: 'error', message: `${errorDetails(error).message} 输入已保留。` }));
  };

  const createProject = () => {
    const body = { name: projectName.trim(), ...(projectDescription.trim() ? { description: projectDescription.trim() } : {}) };
    if (!body.name) return;
    const scope = 'create-project';
    const key = requestKeyFor(scope, body);
    void runAction({
      action: 'create-project', run: () => projectApi.createProject(body, key), requestKey: { scope, key },
      onSuccess: (created) => { setSelectedProjectID(created.id); setProjectName(''); setProjectDescription(''); setShowCreateProject(false); setShowProjectDetails(true); },
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
    const body: CreateProjectWorkItemRequest = {
      title: workTitle.trim(),
      ...(workDescription.trim() ? { description: workDescription.trim() } : {}),
      ...(acceptanceCriteria.trim() ? { acceptance_criteria: acceptanceCriteria.split('\n').map((line) => line.trim()).filter(Boolean) } : {}),
      ...(projectWorkItemsV2 ? { category: workCategory, unassigned: true } : {}),
    };
    const scope = `create-work-item:${project.id}`;
    const key = requestKeyFor(scope, body);
    void runAction({
      action: scope, run: () => projectApi.createWorkItem(project.id, body, key), requestKey: { scope, key },
      onSuccess: () => { setWorkTitle(''); setWorkDescription(''); setAcceptanceCriteria(''); setWorkCategory('bug'); },
      successMessage: '服务端已创建工作项；没有因此启动 Task。',
    });
  };

  const claimWorkItem = (item: ProjectWorkItem) => {
    if (!project || !projectWorkItemsV2 || !canEditWork || item.assignee_id !== null) return;
    const body = { expected_version: item.version };
    const scope = `claim-work-item:${project.id}:${item.id}`;
    const key = requestKeyFor(scope, body);
    void runAction({
      action: scope, run: () => projectApi.claimWorkItem(project.id, item.id, body, key), requestKey: { scope, key },
      successMessage: '服务端已确认认领工作项。',
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
    {notice?.kind === 'error' ? <FloatingError>{notice.message}</FloatingError> : notice && <FloatingNotice className={notice.kind === 'success' ? 'notice-success' : 'notice-warning'}>{notice.message}</FloatingNotice>}

    {!showProjectDetails && <section className="connection-section" aria-labelledby="project-select-title">
      <div className="connection-section-heading"><div><h2 id="project-select-title">我的项目</h2><p className="muted">个人会话仍属于个人，不会随项目成员共享。</p></div><button type="button" className="button button-primary" aria-expanded={showCreateProject} disabled={Boolean(pendingAction)} onClick={() => setShowCreateProject((open) => !open)}>{showCreateProject ? '收起' : '＋ 创建项目'}</button></div>
      {projectsQuery.isPending ? <p role="status">正在读取项目…</p> : projectsQuery.isError ? <FloatingError className="error-card" role="alert">读取项目失败：{errorDetails(projectsQuery.error).message}<button type="button" className="button button-secondary" onClick={() => void projectsQuery.refetch()}>重试</button></FloatingError> : <>
        {projects.length > 0 && <ScreenPagination
          ariaLabel="项目列表"
          items={projects}
          getKey={(item) => item.id}
          estimatedItemHeight={76}
          height={300}
          renderItem={(item) => <article className="schedule-card">
            <div className="schedule-card-heading"><div><h3>{item.name}</h3><p className="muted">{item.description || '暂无说明'}</p></div><button type="button" className="button button-secondary button-small" aria-label={`查看项目 ${item.name}`} disabled={Boolean(pendingAction)} onClick={() => {
            setSelectedProjectID(item.id);
            setSelectedProjectConversationID('');
            setProjectHostID('');
            setProjectWorkspaceID('');
            setProjectMessageReceipt(null);
            setShowProjectDetails(true);
          }}>查看项目</button></div>
          </article>}
        />}
        {!projects.length && <div className="empty-card compact"><p>当前账号还没有项目。</p></div>}
      </>}
      {showCreateProject && <form onSubmit={(event) => { event.preventDefault(); createProject(); }} className="form-stack">
        <label htmlFor="new-project-name">新项目名称</label>
        <input id="new-project-name" value={projectName} maxLength={120} onChange={(event) => setProjectName(event.target.value)} placeholder="例如：客户交付" required />
        <label htmlFor="new-project-description">项目说明</label>
        <textarea id="new-project-description" value={projectDescription} maxLength={2000} onChange={(event) => setProjectDescription(event.target.value)} rows={2} />
        <div className="form-actions"><button type="submit" className="button button-primary" disabled={!projectName.trim() || Boolean(pendingAction)}>{pendingAction === 'create-project' ? '正在创建…' : '创建项目'}</button></div>
      </form>}
    </section>}

    {!showProjectDetails && <section className="connection-section" aria-labelledby="project-invitations-title">
      <div className="connection-section-heading"><div><h2 id="project-invitations-title">发给我的项目邀请</h2><p className="muted">邀请响应只提供项目 ID、邀请人 ID 和角色；服务端没有返回名称或邮箱。</p></div></div>
      {invitationsQuery.isPending ? <p role="status">正在读取邀请…</p> : invitationsQuery.isError ? <FloatingError className="error-inline" role="alert">读取邀请失败：{errorDetails(invitationsQuery.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void invitationsQuery.refetch()}>重试</button></FloatingError> : !invitations.length ? <p className="muted">没有收到邀请。</p> : <ScreenPagination
        ariaLabel="收到的项目邀请" items={invitations} getKey={(invitation) => invitation.id} estimatedItemHeight={86} height={Math.min(300, Math.max(110, invitations.length * 108))}
        renderItem={(invitation) => <article className="schedule-card">
          <div className="schedule-card-heading"><div><h3>项目 {invitation.project_id}</h3><p className="muted">邀请人 {invitation.inviter_id} · {roleLabel(invitation.role)} · v{invitation.version}</p></div><span className="status">{invitationStateLabel(invitation)}</span></div>
          {invitation.state === 'pending' && <button type="button" className="button button-secondary" disabled={Boolean(pendingAction)} onClick={() => acceptInvitation(invitation)}>{pendingAction === `accept-invitation:${invitation.id}` ? '正在接受…' : '接受邀请'}</button>}
        </article>}
      />}
    </section>}

    {showProjectDetails && <section className="design-panel" aria-label="项目详情">
      <div className="design-panel-header"><div><h2>{project?.name || '项目详情'}</h2>{project && <p>{projectStateLabel(project)}</p>}</div><button type="button" className="button button-secondary" disabled={Boolean(pendingAction)} onClick={() => setShowProjectDetails(false)}>← 返回项目</button></div>
      <div className="notice notice-info" role="note">界面只按服务端响应更新；pending、blocked 和 unknown 会保留为服务端状态。工作项创建不会启动 Task；项目共享对话只有在明确提交提示后才排队。</div>
      {projectQuery.isPending && <p role="status">正在读取项目详情…</p>}
      {projectQuery.isError && <FloatingError className="error-inline" role="alert">读取项目详情失败：{errorDetails(projectQuery.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void projectQuery.refetch()}>重试</button></FloatingError>}
    </section>}

    {showProjectDetails && project && <>
      <section className="connection-section" aria-labelledby="project-members-title">
        <div className="connection-section-heading"><div><h2 id="project-members-title">成员与负责人</h2><p className="muted">接口只接收现有账号 ID；项目角色不自动授予 Host 或 Workspace 权限。</p></div></div>
        <p>当前负责人：<strong>{project.owner_id}</strong> · {projectStateLabel(project)}</p>
        <form onSubmit={(event) => { event.preventDefault(); inviteMember(); }} className="inline-form">
          <div><label htmlFor="invite-user-id">被邀请账号 ID</label><input id="invite-user-id" value={inviteUserID} onChange={(event) => setInviteUserID(event.target.value)} placeholder="u 加 7 位小写字母或数字" pattern="u[a-z0-9]{7}" maxLength={8} required /></div>
          <div><label htmlFor="invite-role">项目角色</label><select id="invite-role" value={inviteRole} onChange={(event) => setInviteRole(event.target.value as Exclude<ProjectRole, 'owner'>)}><option value="member">成员</option><option value="viewer">只读成员</option></select></div>
          <button type="submit" className="button button-secondary" disabled={!isOwner || !project || Boolean(pendingAction)}>{pendingAction === `invite:${project.id}` ? '正在邀请…' : '创建邀请'}</button>
        </form>
        {!isOwner && <p className="muted">只有当前负责人可以邀请、移除成员、改角色或移交负责人。</p>}
        {membersQuery.isPending ? <p role="status">正在读取成员…</p> : membersQuery.isError ? <FloatingError className="error-inline" role="alert">读取成员失败：{errorDetails(membersQuery.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void membersQuery.refetch()}>重试</button></FloatingError> : <>
          {members.length ? <ScreenPagination
            ariaLabel="项目成员" items={members} getKey={(member) => member.user_id} estimatedItemHeight={96} height={340}
            renderItem={(member) => <article className="schedule-card">
            <div className="schedule-card-heading"><div><h3>{member.user_id}</h3><p className="muted">{roleLabel(member.role)} · 成员版本 v{member.version}</p></div><span className={`status ${member.role === 'owner' ? 'status-running' : ''}`}>{roleLabel(member.role)}</span></div>
            {isOwner && member.role !== 'owner' && <div className="schedule-actions">
              <label htmlFor={`role-${member.user_id}`}>角色</label>
              <select id={`role-${member.user_id}`} value={member.role} disabled={Boolean(pendingAction)} onChange={(event) => updateMemberRole(member, event.target.value as Exclude<ProjectRole, 'owner'>)}><option value="member">成员</option><option value="viewer">只读成员</option></select>
              <button type="button" className="button button-danger-quiet" disabled={Boolean(pendingAction)} onClick={() => removeMember(member)}>撤销成员</button>
            </div>}
            </article>}
          /> : <p className="muted">服务端尚未返回成员。</p>}
        </>}

        {isOwner && <div className="inline-form">
          <div><label htmlFor="owner-transfer-target">移交负责人给已接受成员</label><select id="owner-transfer-target" value={ownerTargetID} onChange={(event) => setOwnerTargetID(event.target.value)}>
            <option value="">选择成员</option>{members.filter((member) => member.role !== 'owner').map((member) => <option key={member.user_id} value={member.user_id}>{member.user_id}（{roleLabel(member.role)}）</option>)}
          </select></div>
          <button type="button" className="button button-secondary" disabled={!ownerTargetID || Boolean(pendingAction)} onClick={transferOwner}>{pendingAction === `transfer-owner:${project.id}` ? '正在移交…' : '移交负责人'}</button>
        </div>}

        <div className="schedule-list" aria-label="本次页面创建的待处理邀请">
          <h3>本次页面创建的待处理邀请</h3>
          <p className="muted">OpenAPI 没有按项目列出已发邀请的 GET 路由。这里仅显示本次页面拿到 POST 响应的邀请；刷新后无法重建这份列表。</p>
          {sessionInvitations.length ? <ScreenPagination
            ariaLabel="本页创建的项目邀请" items={sessionInvitations} getKey={(invitation) => invitation.id} estimatedItemHeight={84} height={260}
            renderItem={(invitation) => <article className="schedule-card">
            <div className="schedule-card-heading"><div><h4>{invitation.invitee_id}</h4><p className="muted">{roleLabel(invitation.role)} · {invitationStateLabel(invitation)} · 邀请版本 v{invitation.version}</p></div></div>
            {invitation.state === 'pending' && <button type="button" className="button button-danger-quiet" disabled={!isOwner || Boolean(pendingAction)} onClick={() => revokeOutgoingInvitation(invitation)}>撤回邀请</button>}
            </article>}
          /> : null}
          {!sessionInvitations.length && <p className="muted">当前页面尚未创建邀请。</p>}
        </div>
      </section>

      <section className="connection-section" aria-labelledby="project-resources-title">
        <div className="connection-section-heading"><div><h2 id="project-resources-title">项目资源关联</h2><p className="muted">关联现有 Host 或 Workspace 不会复制资源，也不会授予项目成员额外权限。</p></div></div>
        {resourcesQuery.isPending ? <p role="status">正在读取资源关联…</p> : resourcesQuery.isError ? <FloatingError className="error-inline" role="alert">读取资源关联失败：{errorDetails(resourcesQuery.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void resourcesQuery.refetch()}>重试</button></FloatingError> : <>
          {resources.length ? <ScreenPagination
            ariaLabel="项目资源" items={resources} getKey={(resource) => `${resource.kind}:${resource.resource_id}`} estimatedItemHeight={92} height={320}
            renderItem={(resource) => <article className="schedule-card">
            <div className="schedule-card-heading"><div><h3>{displayResource(resource)}</h3><p className="muted">关联版本 v{resource.version}</p></div></div>
            {resource.kind === 'workspace' && <button type="button" className="button button-secondary" onClick={() => setWorkspaceFileView({ projectID, workspaceID: resource.resource_id })}>查看文件目录</button>}
            {isOwner && <button type="button" className="button button-danger-quiet" disabled={Boolean(pendingAction) || !project} onClick={() => removeResource(resource)}>移除关联</button>}
            </article>}
          /> : <p className="muted">没有当前账号可独立访问的项目资源关联。</p>}
        </>}
        {visibleWorkspaceFileView && <div className="schedule-card" aria-label="Workspace 文件目录">
          <div className="schedule-card-heading"><div><h3>Workspace 文件目录</h3><p className="muted">仅显示已确认版本的路径、大小和权限位；不会读取文件内容。</p></div>
            <button type="button" className="button button-secondary" onClick={() => setWorkspaceFileView(null)}>关闭目录</button>
          </div>
          {workspaceManifestQuery.isPending ? <p role="status">正在读取 Workspace 文件目录…</p>
            : workspaceManifestQuery.isError ? <FloatingError className="error-inline" role="alert">读取 Workspace 文件目录失败：{errorDetails(workspaceManifestQuery.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void workspaceManifestQuery.refetch()}>重试</button></FloatingError>
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
          ? <FloatingNotice className="notice-warning" data-notice-label="演示模式">本机 MSW 只模拟消息接收；不会预留预算、调用模型或读取、写入文件。</FloatingNotice>
          : <FloatingNotice className="notice-info">项目执行和预算以服务端回执为准；消息受理不代表任务已开始或预算已预留。</FloatingNotice>}
        {!projectTaskMock && (projectTaskCapabilitiesQuery.isPending ? <p role="status">正在确认项目模型执行能力…</p>
            : projectTaskCapabilitiesQuery.isError ? <FloatingError className="error-inline" role="alert">无法确认项目模型执行能力，消息仍可先保存在本机。<button type="button" className="button button-small button-secondary" onClick={() => void projectTaskCapabilitiesQuery.refetch()}>重试</button></FloatingError>
              : !projectTaskExecutionReady && <FloatingNotice className="notice-warning">服务端尚未声明项目模型执行能力；消息仍可先发送或保存在本机。</FloatingNotice>)}
        {!projectTaskMock && projectTaskCapabilitiesQuery.data &&
          !projectTaskCapabilitiesQuery.data.supported_features.includes('conversation_auto_routing_v1') &&
          <FloatingNotice className="notice-warning">服务端尚未确认支持对话自动处理；仍会尝试发送，无法受理的消息会保留在本机。</FloatingNotice>}
        <form onSubmit={(event) => { event.preventDefault(); createProjectConversation(); }} className="inline-form">
          <div><label htmlFor="project-conversation-title-input">新共享对话标题（可选）</label><input id="project-conversation-title-input" value={projectConversationTitle} maxLength={120} onChange={(event) => setProjectConversationTitle(event.target.value)} /></div>
          <button type="submit" className="button button-secondary" disabled={!currentMember || Boolean(pendingAction)}>{pendingAction === `create-project-conversation:${project.id}` ? '正在创建…' : '创建共享对话'}</button>
        </form>
        {!currentMember && !membersQuery.isPending && <p className="muted">只有已接受的项目成员可以创建共享对话或提交提示。</p>}
        {projectConversationsQuery.isPending ? <p role="status">正在读取共享对话…</p>
          : projectConversationsQuery.isError ? <FloatingError className="error-inline" role="alert">读取项目共享对话失败：{errorDetails(projectConversationsQuery.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void projectConversationsQuery.refetch()}>重试</button></FloatingError>
            : projectConversations.length === 0 ? <p className="muted">项目还没有共享对话；创建后即可向所选模型提交提示。</p> : <>
              <div className="inline-form">
                <div><label htmlFor="project-conversation-select">共享对话</label><select id="project-conversation-select" value={selectedProjectConversation?.id ?? ''} disabled={Boolean(pendingAction)} onChange={(event) => {
                  setSelectedProjectConversationID(event.target.value);
                  setProjectMessageReceipt(null);
                }}>
                  {projectConversations.map((conversation) => <option key={conversation.id} value={conversation.id}>{conversation.title}</option>)}
                </select></div>
                {selectedProjectConversation && <span className="muted">创建者 {selectedProjectConversation.owner_id} · 对话 {selectedProjectConversation.id}</span>}
              </div>
              <div className="schedule-list" aria-label="项目共享对话历史">
                <h3>共享历史</h3>
                {projectConversationMessagesQuery.isPending ? <p role="status">正在读取共享历史…</p>
                  : projectConversationMessagesQuery.isError ? <FloatingError className="error-inline" role="alert">读取共享历史失败：{errorDetails(projectConversationMessagesQuery.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void projectConversationMessagesQuery.refetch()}>重试</button></FloatingError>
                    : !projectConversationMessages.length ? <p className="muted">该共享对话暂无消息。</p>
                      : <ScreenPagination
                        ariaLabel="项目共享对话消息" items={visibleProjectConversationMessages} getKey={(message) => message.id}
                        estimatedItemHeight={130} height={620}
                        renderItem={(message) => {
                        const variantGroupID = projectVariantTimeline.groupByMessageID.get(message.id);
                        const variantGroup = variantGroupID ? projectVariantTimeline.groups.get(variantGroupID) : undefined;
                        const selectedVariantVersion = variantGroup ? selectedMessageVariantVersions[variantGroup.id] ?? variantGroup.latestVersion : 1;
                        const selectedVariantUser = variantGroup?.versions.get(selectedVariantVersion)?.messages.find((candidate) => candidate.role === 'user') as ProjectVersionableMessage | undefined;
                        const canOperateVariant = currentMember?.role !== 'viewer' && selectedVariantUser?.actor_id === user?.id;
                        const groupOperations = variantGroup ? variantOutbox.entries.filter((entry) => entry.operation.input.projectID === project.id &&
                          entry.operation.input.conversationID === message.conversation_id &&
                          (entry.operation.input.messageID === variantGroup.id || variantGroup.messageIDs.has(entry.operation.input.messageID))) : [];
                        const editingThisMessage = editingMessageVariant?.messageID === message.id;
                        const task = taskByID.get(message.task_id ?? message.related_task_id ?? '');
                        const retryAccepted = retryAcceptedMessageIDs.has(message.id);
                        const retryAcceptedStatus = retryAccepted && ['failed', 'pending', 'processing'].includes(message.routing_status ?? '')
                          ? '正在重新处理消息' : null;
                        const messageStatus = message.duplicate_of_message_id
                          ? `重复请求 · 原消息 ${message.duplicate_of_message_id}${message.related_task_id
                            ? ` · Task ${message.related_task_id} · 状态 ${task?.status ?? '待核对'}` : ''}`
                          : message.role === 'user' ? retryAcceptedStatus ?? (message.delivery_status === 'applied' ? deliveryStatusText('applied') : null)
                            : routingStatusText(message.routing_status) ?? deliveryStatusText(message.delivery_status);
                        const sources = message.role === 'assistant' ? message.sources ?? [] : [];
                        const sourceContentMatches = sources.length > 0 && sources.every((source) => source.content.length > 0) &&
                          sources.map((source) => source.content).join('\n\n') === message.content;
                        return <article id={`project-conversation-message-${encodeURIComponent(message.id)}`} className="schedule-card" aria-label="项目对话消息">
                          <div className="schedule-card-heading">
                            {message.role === 'user' && message.routing_status === 'failed' && !message.task_id && !message.related_task_id &&
                              message.actor_id === user?.id && message.routing_failure_reason !== 'message_too_long' && !retryAccepted && <button type="button"
                                className="message-retry" aria-label="重新处理这条消息" title="重新处理"
                                style={{ flex: '0 0 22px', width: 22, height: 22, margin: '2px 0 0', padding: 0, display: 'grid', placeItems: 'center', border: '1px solid #f0b8b4', borderRadius: '50%', color: '#c72f2b', background: '#fff5f4', fontSize: 14, fontWeight: 800, lineHeight: 1, cursor: 'pointer' }}
                                disabled={retryProjectProcessing.isPending && retryProjectProcessing.variables?.messageID === message.id}
                                onClick={() => retryProjectProcessing.mutate({ projectID: project.id, conversationID: message.conversation_id, messageID: message.id })}>!</button>}
                            <div><h4>{message.role === 'user' ? '成员提示' : 'AI 回复'}</h4><p className="muted">执行者 {message.actor_id} · 对话创建者 {message.owner_id}</p></div>
                            {(message.output_status === 'streaming' || message.output_status === 'interrupted' || messageStatus) &&
                              <span className="status">{message.output_status === 'streaming' ? '生成中' : message.output_status === 'interrupted' ? '已中断' : messageStatus}</span>}
                          </div>
                          {editingThisMessage ? <form className="message-variant-edit" onSubmit={(event) => {
                            event.preventDefault();
                            if (variantGroupID && editingMessageVariant) void startProjectVariantOperation(variantGroupID, message.id, 'edit_resend', editingMessageVariant.content);
                          }}>
                            <label className="sr-only" htmlFor={`project-message-version-edit-${encodeURIComponent(message.id)}`}>编辑项目消息并创建新版本</label>
                            <textarea id={`project-message-version-edit-${encodeURIComponent(message.id)}`} value={editingMessageVariant.content}
                              maxLength={maxMessageVariantScalars * 2}
                              onChange={(event) => setEditingMessageVariant((current) => current?.messageID === message.id ? { ...current, content: event.target.value } : current)} />
                            {messageVariantContentTooLong(editingMessageVariant.content) && <small role="alert">编辑后的消息不能超过 {maxMessageVariantScalars} 个字符。</small>}
                            <div className="message-variant-control-actions">
                              <button type="submit" className="button button-small button-primary" disabled={!editingMessageVariant.content.trim() || messageVariantContentTooLong(editingMessageVariant.content) || groupOperations.some((entry) => entry.operation.status === 'sending')}>创建新版本</button>
                              <button type="button" className="button button-small button-secondary" onClick={() => setEditingMessageVariant(null)}>取消</button>
                            </div>
                          </form> : sourceContentMatches ? <div className="chat-message-source-list">
                            {sources.map((source) => {
                              const sourceMessage = projectConversationMessages.find((candidate) => candidate.id === source.user_message_id);
                              const prompt = sourceMessage?.content.trim().replace(/\s+/g, ' ');
                              const excerpt = prompt && [...prompt].length > 72 ? `${[...prompt].slice(0, 72).join('')}…` : prompt;
                              return <section className="chat-message-source" key={`${source.task_id}:${source.task_revision}:${source.event_sequence}`}>
                                <div className="chat-message-source-meta">
                                  {sourceMessage ? <a className="chat-message-source-origin"
                                    href={`#project-conversation-message-${encodeURIComponent(source.user_message_id)}`}
                                    title={prompt} aria-label={`查看关联的成员消息：${prompt}`}>
                                    {excerpt ? `回复消息：${excerpt}` : '查看关联消息'}
                                  </a> : <span className="chat-message-source-origin">关联消息 {source.user_message_id}</span>}
                                  <span className="chat-message-source-task">任务 {source.task_id} · 修订 {source.task_revision} · 结果事件 {source.event_sequence}</span>
                                </div>
                                <ChatMarkdown content={source.content} />
                              </section>;
                            })}
                          </div> : <>
                            {message.role === 'assistant' ? <ChatMarkdown content={message.content} /> : <p>{message.content}</p>}
                            {sources.length > 0 && <div className="chat-message-source-meta" aria-label="回复来源">
                              {sources.map((source) => <span className="chat-message-source-task" key={`${source.task_id}:${source.task_revision}:${source.event_sequence}`}>
                                关联消息 {source.user_message_id} · 任务 {source.task_id} · 修订 {source.task_revision} · 结果事件 {source.event_sequence}
                              </span>)}
                            </div>}
                          </>}
                          {message.role === 'assistant' && message.output_status !== 'streaming' && message.output_status !== 'interrupted' &&
                            <AssistantReplySpeech text={message.content} sessionKey={`project:${project.id}:conversation:${message.conversation_id}`} />}
                          {message.attachment_ids?.length ? <p className="muted">附件 · {message.attachment_ids.length}
                            {message.attachment_ids.map((attachmentID, index) => <span key={attachmentID}> · <a
                              href={`/api/projects/${encodeURIComponent(project.id)}/conversations/${encodeURIComponent(message.conversation_id)}/messages/${encodeURIComponent(message.id)}/attachments-by-id/${encodeURIComponent(attachmentID)}/content`}
                              download>下载附件 {index + 1}</a></span>)}
                          </p> : null}
                          {message.output_status === 'streaming' && <small role="status">正在生成…</small>}
                          {message.output_status === 'interrupted' && <small role="status">回复已中断</small>}
                          {variantGroup && <div className="message-variant-controls" aria-label="项目消息版本操作">
                            {message.role === 'user' && variantGroup.versions.size > 1 && <label className="message-variant-picker">版本
                              <select aria-label="选择项目消息版本" value={selectedVariantVersion} onChange={(event) => setSelectedMessageVariantVersions((current) => ({
                                ...current, [variantGroup.id]: Number(event.target.value),
                              }))}>
                                {[...variantGroup.versions.values()].sort((left, right) => left.version - right.version).map((version) => <option key={version.version} value={version.version}>
                                  {`v${version.version} · ${messageVariantActionLabel(version.action)}`}
                                </option>)}
                              </select>
                            </label>}
                            {message.role === 'user' && message.actor_id === user?.id && currentMember && currentMember.role !== 'viewer' && !editingThisMessage && <button type="button" className="button button-small button-secondary"
                              disabled={groupOperations.some((entry) => entry.operation.status === 'sending')}
                              onClick={() => setEditingMessageVariant({ groupID: variantGroup.id, messageID: message.id, content: message.content })}>编辑并重发</button>}
                            {message.role === 'assistant' && message.content.trim() && message.output_status !== 'streaming' && canOperateVariant && <button type="button"
                              className="button button-small button-secondary" disabled={groupOperations.some((entry) => entry.operation.status === 'sending')}
                              onClick={() => void startProjectVariantOperation(variantGroup.id, message.id, 'regenerate_text')}>重新生成文字</button>}
                            {message.role === 'assistant' && message.output_status === 'interrupted' && canOperateVariant && <button type="button"
                              className="button button-small button-secondary" disabled={groupOperations.some((entry) => entry.operation.status === 'sending')}
                              onClick={() => void startProjectVariantOperation(variantGroup.id, message.id, 'continue_generation')}>继续生成</button>}
                          </div>}
                          {message.role === 'user' && groupOperations.map((entry) => <div className="message-variant-operation" role="status" key={entry.operation.input.idempotencyKey}>
                            <span>{entry.operation.status === 'sending' ? '正在创建消息版本…' : entry.operation.status === 'unknown'
                              ? `操作结果待核对：${entry.operation.error ?? ''}` : entry.operation.error ?? '消息版本操作待发送。'}</span>
                            {(entry.operation.status === 'unknown' || entry.operation.status === 'pending') && <button type="button" className="button button-small button-secondary"
                              disabled={groupOperations.some((item) => item.operation.status === 'sending')} onClick={() => void sendProjectVariantOperation(entry)}>核对并重试</button>}
                          </div>)}
                          {message.role === 'user' && message.routing_status === 'failed' && message.routing_failure_reason === 'message_too_long' &&
                            <p role="alert" className="muted">{routingFailureText(message.routing_failure_reason)}</p>}
                          {message.task_id && <p className="muted">Task {message.task_id} · 状态 {task?.status ?? '未知'}
                            {task && Number.isSafeInteger(task.step_count) && task.step_count! > 0 && Number.isSafeInteger(task.completed_step_count) &&
                              <> · 已完成 {Math.min(task.completed_step_count!, task.step_count!)} / {task.step_count!} 步</>}
                            {task && task.initiator_id === user?.id && ['queued', 'running', 'waiting_user', 'paused'].includes(task.status) &&
                              <button type="button" className="button button-small button-secondary" disabled={Boolean(pendingAction)}
                                onClick={() => stopProjectTask(task)}>停止此任务</button>}
                          </p>}
                        </article>;
                      }}
                      />}
                {pendingProjectMessages.length > 0 && <ScreenPagination
                  ariaLabel="待发送项目消息" items={pendingProjectMessages} getKey={({ message }) => message.request.client_message_id}
                  estimatedItemHeight={96} height={280}
                  renderItem={({ scope, message }) => <article className="schedule-card" aria-label="待发送项目消息">
                  <p>{message.request.content}</p><p role="status">{message.status === 'sending' ? '正在发送' : message.status === 'unknown' ? '发送结果待确认' : message.status === 'failed' ? '发送失败' : '已保存在本机，等待发送'}{message.error ? `：${message.error}` : ''}</p>
                  {message.status !== 'sending' && <button type="button" className="button button-small button-secondary" onClick={() => void sendProjectMessage({ scope, message }, true)}>核对并重试</button>}
                  </article>}
                />}
              </div>
              <form onSubmit={(event) => { event.preventDefault(); queueProjectMessage(); }} className="form-stack">
                <label htmlFor="project-task-model">文本模型</label>
                <select id="project-task-model" value={effectiveProjectModelID} disabled={modelConnectionsQuery.isPending} onChange={(event) => setProjectModelID(event.target.value)}>
                  <option value="">自动选择模型</option>
                  {effectiveProjectModelID && !readyTextModels.some((model) => model.id === effectiveProjectModelID) && <option value={effectiveProjectModelID}>已选择模型，状态待核对</option>}
                  {readyTextModels.map((model) => <option key={model.id} value={model.id}>{model.name} · {model.model_name}</option>)}
                </select>
                {modelConnectionsQuery.isPending ? <p role="status">正在读取可用模型…</p>
                  : modelConnectionsQuery.isError ? <FloatingError className="error-inline" role="alert">读取模型失败：{errorDetails(modelConnectionsQuery.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void modelConnectionsQuery.refetch()}>重试</button></FloatingError>
                    : !readyTextModels.length && <p className="muted">当前没有已验证的文字模型；消息可先发送，处理状态以服务端为准。</p>}
                {outbox.storageError && <FloatingError className="error-inline" role="alert">本地消息存储不可用：{outbox.storageError}</FloatingError>}
                {variantOutbox.storageError && <FloatingError className="error-inline" role="alert">本地项目消息版本操作存储不可用：{variantOutbox.storageError}</FloatingError>}
                <div className="inline-form">
                  <div><label htmlFor="project-task-host">目标电脑（可选）</label><select id="project-task-host" value={projectHostID} disabled={!resourcesQuery.isSuccess || taskHostsQuery.isPending} onChange={(event) => {
                    setProjectHostID(event.target.value);
                    setProjectWorkspaceID('');
                  }}>
                    <option value="">云端 Task（不指定电脑）</option>
                    {projectHostID && !selectedTaskHost && <option value={projectHostID}>所选电脑状态待核对</option>}{taskHosts.map((host) => <option key={host.id} value={host.id}>{host.name} · {host.id}</option>)}
                  </select></div>
                  <div><label htmlFor="project-task-workspace">输入 Workspace（可选）</label><select id="project-task-workspace" value={projectWorkspaceID} disabled={!resourcesQuery.isSuccess || taskWorkspacesQuery.isPending} onChange={(event) => setProjectWorkspaceID(event.target.value)}>
                    <option value="">不指定 Workspace</option>
                    {projectWorkspaceID && !selectedTaskWorkspace && <option value={projectWorkspaceID}>所选 Workspace 状态待核对</option>}{taskWorkspaces.map((workspace) => <option key={workspace.id} value={workspace.id}>{workspace.name} · revision {workspace.revision}</option>)}
                  </select></div>
                </div>
                {resourcesQuery.isPending || taskHostsQuery.isPending || taskWorkspacesQuery.isPending
                  ? <p role="status">正在核实项目关联资源和当前账号的独立访问权限…</p>
                  : resourcesQuery.isError || taskHostsQuery.isError || taskWorkspacesQuery.isError
                    ? <p className="muted">部分目标资源状态暂不可用，因此未开放对应 Host/Workspace 选择；可提交不指定电脑或 Workspace 的 Task。</p>
                    : !taskHosts.length && !taskWorkspaces.length
                      ? <p className="muted">没有当前账号可独立访问且已关联到本项目的就绪 Host/Workspace；项目关联本身不会授予资源访问权。</p>
                      : selectedTaskHost && !taskWorkspaces.length && <p className="muted">可选 Workspace 必须属于本项目、当前账号可独立访问，并已在所选电脑应用到当前 revision。</p>}
                {invalidTaskTargetSelection && <p className="form-error" role="status">所选目标状态待核对；实际使用时会检查权限和状态，消息仍可发送。</p>}
                <label htmlFor="project-task-prompt">提交到共享对话的提示</label>
                <textarea id="project-task-prompt" value={projectPrompt} rows={4} onChange={(event) => setProjectPrompt(event.target.value)} required />
                <div className="form-actions">
                  <AssistantVoiceInput onTranscript={(transcript) => setProjectPrompt((current) => current.trim()
                    ? `${current.trimEnd()} ${transcript}` : transcript)} disabled={Boolean(pendingAction)} />
                  <button type="submit" className="button button-primary" disabled={!selectedProjectConversation || !projectPrompt.trim()}>发送消息</button>
                </div>
              </form>
              {visibleProjectMessageReceipt && <p className="muted" role="status">服务端已受理消息，等待分流。</p>}
            </>}
      </section>

      <section className="connection-section" aria-labelledby="project-budget-title">
        <div className="connection-section-heading"><div><h2 id="project-budget-title">项目预算与账本</h2><p className="muted">金额使用整数微单位；未知费用按接口状态显示，不在浏览器估算或清零。</p></div></div>
        {budgetQuery.isPending ? <p role="status">正在读取预算…</p> : budgetQuery.isError ? <FloatingError className="error-inline" role="alert">读取预算失败：{errorDetails(budgetQuery.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void budgetQuery.refetch()}>重试</button></FloatingError> : budget && <>
          <div className="notice notice-info" role="status">
            {budget.configured ? <>额度 {safeMoney(budget.limit_micros, budget.currency)} · 已结算 {safeMoney(budget.spent_micros, budget.currency)} · 已预留 {safeMoney(budget.reserved_micros, budget.currency)} · 预算版本 v{budget.version}</> : '尚未配置项目预算。'}
            {budget.has_unknown && <strong> 存在未知费用，需等待服务端对账。</strong>}
          </div>
          <p className="muted">{projectTaskMock
            ? '本机 MSW 只展示合成预算归因；保存限额不会预留真实模型费用或启动 Task。'
            : '此处展示服务端返回的预算与账本状态；未知费用等待服务端对账。'}</p>
          <form onSubmit={(event) => { event.preventDefault(); setProjectBudget(); }} className="inline-form">
            <div><label htmlFor="budget-currency">币种（3 位大写代码）</label><input id="budget-currency" value={budgetCurrency} maxLength={3} onChange={(event) => setBudgetCurrency(event.target.value.toUpperCase())} pattern="[A-Z]{3}" required /></div>
            <div><label htmlFor="budget-limit">预算限额（主币种单位，最多 6 位小数）</label><input id="budget-limit" inputMode="decimal" value={budgetLimit} onChange={(event) => setBudgetLimit(event.target.value)} placeholder="10.00" required /></div>
            <button type="submit" className="button button-secondary" disabled={!isOwner || !canSubmitBudget || Boolean(pendingAction)}>{pendingAction === `set-budget:${project.id}` ? '正在保存…' : '保存预算'}</button>
          </form>
          {!isOwner && <p className="muted">只有负责人可以修改预算。</p>}
        </>}
        <h3>服务端账本</h3>
        {ledgerQuery.isPending ? <p role="status">正在读取账本…</p> : ledgerQuery.isError ? <FloatingError className="error-inline" role="alert">读取账本失败：{errorDetails(ledgerQuery.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void ledgerQuery.refetch()}>重试</button></FloatingError> : !ledger.length ? <p className="muted">服务端尚无账本条目。</p> : <ScreenPagination
          ariaLabel="项目账本" items={ledger} getKey={(entry) => entry.call_id} estimatedItemHeight={120} height={340}
          renderItem={(entry) => <article className="schedule-card">
            <div className="schedule-card-heading"><div><h4>{entry.source} · {entry.model_id}</h4><p className="muted">调用 {entry.call_id} · 执行者 {entry.actor_id} · 费率版本 v{entry.rate_version}</p></div><span className={`status ${entry.status === 'settled' ? 'status-running' : entry.status === 'unknown' ? 'status-failed' : ''}`}>{entry.status}</span></div>
            <p>预留 {safeMoney(entry.reserved_micros, entry.currency)} · 实际 {entry.actual_micros === undefined ? (entry.status === 'unknown' ? '未知' : '未结算') : safeMoney(entry.actual_micros, entry.currency)}</p>
            <p className="muted">预留用量：输入 {entry.reserved_usage.input_tokens.toLocaleString('zh-CN')} / 输出 {entry.reserved_usage.output_tokens.toLocaleString('zh-CN')} tokens · {usageLabel(entry)}</p>
            {entry.task_id && <p className="muted">Task：{entry.task_id}</p>}
          </article>}
        />}
      </section>

      <section className="connection-section" aria-labelledby="project-work-title">
        <div className="connection-section-heading"><div><h2 id="project-work-title">项目 Todo 与工作项</h2><p className="muted">只显示服务端状态。创建工作项不启动执行；Task 必须先存在并通过服务端授权后再关联。</p></div></div>
        <form onSubmit={(event) => { event.preventDefault(); createWorkItem(); }} className="form-stack">
          {projectWorkItemsV2 && <><label htmlFor="work-category">类型</label><select id="work-category" value={workCategory} onChange={(event) => setWorkCategory(event.target.value as ProjectWorkItemCategory)}>
            <option value="bug">缺陷</option><option value="request">需求</option>
          </select></>}
          <label htmlFor="work-title">工作项标题</label><input id="work-title" value={workTitle} maxLength={200} onChange={(event) => setWorkTitle(event.target.value)} required />
          <label htmlFor="work-description">说明</label><textarea id="work-description" value={workDescription} maxLength={4096} rows={3} onChange={(event) => setWorkDescription(event.target.value)} />
          <label htmlFor="work-criteria">验收标准（每行一项）</label><textarea id="work-criteria" value={acceptanceCriteria} rows={3} onChange={(event) => setAcceptanceCriteria(event.target.value)} />
          <div className="form-actions"><button type="submit" className="button button-primary" disabled={!canManageWork || !workTitle.trim() || Boolean(pendingAction)}>{pendingAction === `create-work-item:${project.id}` ? '正在创建…' : '创建工作项'}</button></div>
        </form>
        {!canEditWork && <p className="muted">当前成员角色不能修改工作项；等待角色数据或使用负责人/成员账号。</p>}

        {projectWorkItemsV2 && <div className="form-actions" role="group" aria-label="工作项筛选">
          {([['all', '全部'], ['bug', '缺陷'], ['request', '需求'], ['unclaimed', '待认领']] as const).map(([filter, label]) => <button key={filter} className={`button button-small ${workItemFilter === filter ? 'button-primary' : 'button-secondary'}`} type="button" aria-pressed={workItemFilter === filter} onClick={() => setWorkItemFilter(filter)}>{label}</button>)}
        </div>}
        {workItemsQuery.isPending ? <p role="status">正在读取工作项…</p> : workItemsQuery.isError ? <FloatingError className="error-inline" role="alert">读取工作项失败：{errorDetails(workItemsQuery.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void workItemsQuery.refetch()}>重试</button></FloatingError> : <>
          {visibleWorkItems.length ? <ScreenPagination
            key={workItemFilter} ariaLabel="项目工作项" items={visibleWorkItems} getKey={(item) => item.id}
            estimatedItemHeight={420} height="min(74dvh, 700px)"
            renderItem={(item) => {
            const linkedTaskIDs = new Set(item.task_ids);
            const taskCandidates = tasks.filter((task) => !linkedTaskIDs.has(task.id));
            const deliveryChoice = deliveryChoices[item.id] ?? { taskID: item.task_ids[0] ?? '', workspaceID: '', revision: '' };
            return <article className="schedule-card">
              <div className="schedule-card-heading"><div><h3>{item.title}</h3><p className="muted">{item.id} · 工作项版本 v{item.version} · 负责人 {item.assignee_id ?? '待认领'}</p>{projectWorkItemsV2 && <p className="muted">类型：{workItemCategoryLabel(item.category)}</p>}</div><span className={`status ${item.state === 'open' ? 'status-running' : item.state === 'delivered' ? 'status-completed' : ''}`}>{workItemStateLabel(item)}</span></div>
              {item.description && <p>{item.description}</p>}
              {item.acceptance_criteria.length > 0 && <div><strong>验收标准</strong><ul>{item.acceptance_criteria.map((criterion, index) => <li key={`${index}:${criterion}`}>{criterion}</li>)}</ul></div>}
              {item.blocked_reason && <p className="notice notice-warning">阻塞原因：{item.blocked_reason}</p>}
              <div className="schedule-actions">
                <label htmlFor={`assignee-${item.id}`}>负责人</label>
                <select id={`assignee-${item.id}`} value={item.assignee_id ?? ''} disabled={!canManageWork || Boolean(pendingAction)} onChange={(event) => assignWorkItem(item, event.target.value)}>
                  {item.assignee_id === null && <option value="" disabled>待认领</option>}
                  {members.map((member) => <option key={member.user_id} value={member.user_id}>{member.user_id}</option>)}
                </select>
              </div>
              {projectWorkItemsV2 && item.assignee_id === null && canEditWork && <button type="button" className="button button-secondary" disabled={Boolean(pendingAction)} onClick={() => claimWorkItem(item)}>认领工作项</button>}
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
                {tasksQuery.isError && <FloatingError className="error-inline" role="alert">读取 Task 列表失败：{errorDetails(tasksQuery.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void tasksQuery.refetch()}>重试</button></FloatingError>}
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
          }}
          /> : <div className="empty-card compact"><p>没有符合条件的工作项。</p></div>}
        </>}
      </section>
    </>}
  </section>;
}
