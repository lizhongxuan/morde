import createClient from 'openapi-fetch';
import { ApiError, requestFetch } from './client';
import type { components, paths } from './schema';

export type GatewayHost = {
  id: string;
  name: string;
  portal_origin: string;
  state: 'creating' | 'running' | 'failed' | 'deleting' | 'deleted';
  expires_at: string | null;
  workspaces: Array<{
    workspace: {
      id: string;
      name: string;
      revision: number;
      used_bytes: number;
      state: 'ready' | 'deleting' | 'deleted';
    };
    directory: string;
    state: 'preparing' | 'active' | 'unbinding' | 'detached' | 'failed';
    binding_version: number;
    desired_revision: number;
    applied_revision: number;
  }>;
};

export type GatewayControlAction = 'take_over' | 'release';
export type GatewayControlState = 'ai' | 'taking_over' | 'user' | 'returning';
export type GatewayControl = {
  host_id: string;
  control_state: GatewayControlState;
  control_version: number;
  active_transition: {
    request_id: string;
    action: GatewayControlAction;
    phase: string;
    failure_reason?: string;
    expected_control_version: number;
  } | null;
  pending_operation_ids?: string[];
};
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
  '/__ch/api/renew': { post: JsonResponse<GatewayHost> };
  '/__ch/api/control': { get: JsonResponse<GatewayControl> };
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

export const gatewayApi = {
  async host() {
    return unwrap(await client.GET('/__ch/api/host'));
  },
  async renew() {
    return unwrap(await client.POST('/__ch/api/renew'));
  },
  async control() {
    return unwrap(await client.GET('/__ch/api/control'));
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
