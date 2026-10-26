import type { components } from '../api/schema';

export type PortalUser = components['schemas']['Me'];

export type Host = components['schemas']['Host'];
export type Workspace = components['schemas']['Workspace'];
export type WorkspaceBinding = components['schemas']['WorkspaceBinding'];
export type Template = components['schemas']['Template'];

export type AdminUser = components['schemas']['AdminUser'];

export interface AuditLog {
  id: number;
  at: string;
  actor_id: string;
  actor_kind: string;
  action: string;
  target: string;
  detail: Record<string, unknown>;
  ip: string;
}

export interface Capacity {
  limit: { cpu_milli: number; mem_mib: number };
  allocated: { cpu_milli: number; mem_mib: number };
  hosts_by_state: Record<string, number>;
}

export type Items<T> = { items: T[] };
