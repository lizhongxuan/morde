import createClient from 'openapi-fetch';
import { ApiError, requestFetch } from './client';
import type { components, paths } from './schema';

export type GatewayHost = {
  id: string;
  name: string;
  portal_origin: string;
  state: 'creating' | 'running' | 'failed' | 'deleting' | 'deleted';
  expires_at: string | null;
  workspaces: GatewayWorkspaceBinding[];
  embed_targets?: string[];
};

export type GatewayWorkspaceBinding = {
  workspace: {
    id: string;
    name: string;
    revision: number;
    used_bytes: number;
    quota_bytes: number;
    state: 'ready' | 'deleting' | 'deleted';
  };
  directory: string;
  state: 'preparing' | 'active' | 'unbinding' | 'detached' | 'failed';
  binding_version: number;
  desired_revision: number;
  applied_revision: number;
  sync_state: 'healthy' | 'conflict';
  sync_conflicts: string[];
};

export type GatewayWorkspaceManifest = components['schemas']['WorkspaceManifest'];
export type GatewayWorkspaceDiff = components['schemas']['WorkspaceDiff'];
export type GatewayTaskObservationSnapshot = components['schemas']['TaskWorkbenchObservationSnapshot'];

export type GatewayControlAction = 'take_over' | 'release';
export type GatewayControlState = 'ai' | 'taking_over' | 'user' | 'returning';
export type GatewayControl = components['schemas']['ControlStatus'] & { pending_operation_ids?: string[] };
export type GatewayControlRequest = {
  action: GatewayControlAction;
  request_id: string;
  expected_control_version: number;
};

export type GatewayApplication = components['schemas']['HostApplicationStatusItem'];
export type GatewayApplicationInstallOperation = components['schemas']['HostApplicationInstallOperation'];
export type GatewayApplicationInstallResponse = components['schemas']['HostApplicationInstallResponse'];
export type GatewayApplicationLaunchOperation = components['schemas']['HostApplicationLaunchOperation'];
export type GatewayApplicationLaunchResponse = components['schemas']['HostApplicationLaunchResponse'];
export type GatewayCurrentApplicationLaunchResponse = components['schemas']['HostCurrentApplicationLaunchResponse'];
export type GatewayApplications = components['schemas']['HostApplicationsResponse'];
export type GatewayTerminalSession = {
  state: 'creating' | 'ready' | 'unknown' | 'interrupted';
  url: string;
};

type JsonResponse<T> = { responses: { 200: { content: { 'application/json': T } } } };
type AcceptedJsonResponse<T> = {
  responses: {
    200: { content: { 'application/json': T } };
    202: { content: { 'application/json': T } };
  };
};

type GatewayPaths = {
  '/__ch/api/host': { get: JsonResponse<GatewayHost> };
  '/__ch/api/workspaces': { get: JsonResponse<{ items: GatewayWorkspaceBinding[] }> };
  '/__ch/api/workspaces/{workspace_id}/manifest': {
    get: {
      parameters: {
        path: { workspace_id: string };
        query: { revision: number; prefix?: string; cursor?: string; limit?: number };
      };
      responses: JsonResponse<GatewayWorkspaceManifest>['responses'];
    };
  };
  '/__ch/api/workspaces/{workspace_id}/diff': {
    get: {
      parameters: {
        path: { workspace_id: string };
        query: { from_revision: number; to_revision: number };
      };
      responses: JsonResponse<GatewayWorkspaceDiff>['responses'];
    };
  };
  '/__ch/api/tasks/{task_id}': {
    get: {
      parameters: { path: { task_id: string } };
      responses: JsonResponse<GatewayTaskObservationSnapshot>['responses'];
    };
  };
  '/__ch/api/renew': { post: JsonResponse<GatewayHost> };
  '/__ch/api/control': { get: JsonResponse<GatewayControl> };
  '/__ch/api/control/lease/renew': {
    post: {
      requestBody: { content: { 'application/json': components['schemas']['ControlLeaseRenewRequest'] } };
      responses: JsonResponse<GatewayControl>['responses'];
    };
  };
  '/__ch/api/applications': paths['/__ch/api/applications'];
  '/__ch/api/applications/{app_id}/install': {
    post: {
      parameters: { path: { app_id: string } };
      requestBody: { content: { 'application/json': components['schemas']['HostApplicationInstallRequest'] } };
      responses: { 202: { content: { 'application/json': GatewayApplicationInstallResponse } } };
    };
  };
  '/__ch/api/applications/{app_id}/launch': {
    post: {
      parameters: { path: { app_id: string } };
      requestBody: { content: { 'application/json': components['schemas']['HostApplicationLaunchRequest'] } };
      responses: AcceptedJsonResponse<GatewayApplicationLaunchResponse>['responses'];
    };
  };
  '/__ch/api/applications/{app_id}/launches/current': {
    get: {
      parameters: { path: { app_id: string } };
      responses: JsonResponse<GatewayCurrentApplicationLaunchResponse>['responses'];
    };
  };
  '/__ch/api/applications/{app_id}/launches/{launch_id}': {
    get: {
      parameters: { path: { app_id: string; launch_id: string } };
      responses: JsonResponse<GatewayApplicationLaunchResponse>['responses'];
    };
  };
  '/__ch/api/applications/{app_id}/launches/{launch_id}/stop': {
    post: {
      parameters: { path: { app_id: string; launch_id: string } };
      responses: AcceptedJsonResponse<GatewayApplicationLaunchResponse>['responses'];
    };
  };
  '/__ch/api/terminal-session': {
    put: JsonResponse<GatewayTerminalSession>;
    post: {
      requestBody: { content: { 'application/json': { confirm_new: true } } };
      responses: JsonResponse<GatewayTerminalSession>['responses'];
    };
  };
  '/__ch/api/control/actions': {
    post: {
      requestBody: { content: { 'application/json': GatewayControlRequest } };
      responses: AcceptedJsonResponse<GatewayControl>['responses'];
    };
  };
};

const client = createClient<GatewayPaths>({ baseUrl: window.location.origin, fetch: requestFetch });

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object';
}

function unwrap<T>(result: { data?: T; error?: unknown; response: Response }): T {
  if (result.error !== undefined) {
    const outer = isRecord(result.error) ? result.error : {};
    const raw = isRecord(outer.error) ? outer.error : {};
    const retryText = result.response.headers.get('Retry-After');
    const retry = retryText === null ? undefined : Number(retryText);
    throw new ApiError({
      message: typeof raw.message === 'string' ? raw.message : '请求失败，请稍后重试',
      code: typeof raw.code === 'string' ? raw.code : 'internal',
      status: result.response.status,
      retryAfter: Number.isFinite(retry) ? retry : undefined,
      details: isRecord(raw.details) ? raw.details : {},
    });
  }
  if (result.data === undefined) {
    throw new ApiError({ message: '服务器返回了空响应', code: 'internal', status: result.response.status });
  }
  return result.data;
}

async function throwResponseError(response: Response): Promise<never> {
  const envelope = await response.json().catch(() => null) as unknown;
  const outer = isRecord(envelope) ? envelope : {};
  const raw = isRecord(outer.error) ? outer.error : {};
  const retryText = response.headers.get('Retry-After');
  const retry = retryText === null ? undefined : Number(retryText);
  throw new ApiError({
    message: typeof raw.message === 'string' ? raw.message : `请求失败（HTTP ${response.status}）`,
    code: typeof raw.code === 'string' ? raw.code : 'internal',
    status: response.status,
    retryAfter: Number.isFinite(retry) ? retry : undefined,
    details: isRecord(raw.details) ? raw.details : {},
  });
}

function workspaceFileURL(workspaceID: string, revision: number, relativePath: string): URL {
  const encodedPath = encodeURIComponent(relativePath);
  const url = new URL(`/__ch/api/workspaces/${encodeURIComponent(workspaceID)}/files/${encodedPath}`, window.location.origin);
  url.searchParams.set('revision', String(revision));
  return url;
}

export const gatewayApi = {
  async host() {
    return unwrap(await client.GET('/__ch/api/host'));
  },
  async workspaces() {
    return unwrap(await client.GET('/__ch/api/workspaces'));
  },
  async workspaceManifest(workspaceID: string, revision: number, prefix?: string, cursor?: string, limit = 500) {
    return unwrap(await client.GET('/__ch/api/workspaces/{workspace_id}/manifest', {
      params: {
        path: { workspace_id: workspaceID },
        query: { revision, ...(prefix ? { prefix } : {}), ...(cursor ? { cursor } : {}), limit },
      },
    }));
  },
  async workspaceDiff(workspaceID: string, fromRevision: number, toRevision: number) {
    return unwrap(await client.GET('/__ch/api/workspaces/{workspace_id}/diff', {
      params: { path: { workspace_id: workspaceID }, query: { from_revision: fromRevision, to_revision: toRevision } },
    }));
  },
  async taskObservation(taskID: string) {
    return unwrap(await client.GET('/__ch/api/tasks/{task_id}', { params: { path: { task_id: taskID } } }));
  },
  taskObservationEventsURL(taskID: string, after: number) {
    const url = new URL(`/__ch/api/tasks/${encodeURIComponent(taskID)}/events`, window.location.origin);
    url.searchParams.set('after', String(after));
    return url.href;
  },
  async downloadWorkspaceFile(workspaceID: string, revision: number, relativePath: string) {
    const response = await requestFetch(workspaceFileURL(workspaceID, revision, relativePath), { method: 'GET', credentials: 'same-origin' });
    if (!response.ok) await throwResponseError(response);
    const objectURL = URL.createObjectURL(await response.blob());
    const link = document.createElement('a');
    link.href = objectURL;
    link.download = relativePath.split('/').at(-1) || 'download';
    link.hidden = true;
    document.body.append(link);
    try {
      link.click();
    } finally {
      link.remove();
      window.setTimeout(() => URL.revokeObjectURL(objectURL), 1000);
    }
  },
  async renew() {
    return unwrap(await client.POST('/__ch/api/renew'));
  },
  async control() {
    return unwrap(await client.GET('/__ch/api/control'));
  },
  async renewControlLease(expected_control_version: number) {
    return unwrap(await client.POST('/__ch/api/control/lease/renew', { body: { expected_control_version } }));
  },
  async applications() {
    return unwrap(await client.GET('/__ch/api/applications'));
  },
  async installApplication(appId: string, request: { request_id: string }) {
    return unwrap(await client.POST('/__ch/api/applications/{app_id}/install', {
      params: { path: { app_id: appId } },
      body: request,
    }));
  },
  async currentApplicationLaunch(appId: string) {
    return unwrap(await client.GET('/__ch/api/applications/{app_id}/launches/current', {
      params: { path: { app_id: appId } },
    }));
  },
  async launchApplication(appId: string, request: { launch_id: string }) {
    return unwrap(await client.POST('/__ch/api/applications/{app_id}/launch', {
      params: { path: { app_id: appId } },
      body: request,
    }));
  },
  async applicationLaunch(appId: string, launchId: string) {
    return unwrap(await client.GET('/__ch/api/applications/{app_id}/launches/{launch_id}', {
      params: { path: { app_id: appId, launch_id: launchId } },
    }));
  },
  async stopApplicationLaunch(appId: string, launchId: string) {
    return unwrap(await client.POST('/__ch/api/applications/{app_id}/launches/{launch_id}/stop', {
      params: { path: { app_id: appId, launch_id: launchId } },
    }));
  },
  async ensureTerminalSession() {
    return unwrap(await client.PUT('/__ch/api/terminal-session'));
  },
  async newTerminalSession() {
    return unwrap(await client.POST('/__ch/api/terminal-session', { body: { confirm_new: true } }));
  },
  async controlAction(body: GatewayControlRequest) {
    return unwrap(await client.POST('/__ch/api/control/actions', { body }));
  },
};
