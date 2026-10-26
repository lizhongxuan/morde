import type { components } from '../api/schema';

export interface PortalUser {
  id: string;
  name: string;
  role: string;
  disabled?: boolean;
  quota?: { hosts: number; cpu_milli: number; mem_mib: number };
  usage?: { hosts: number; cpu_milli: number; mem_mib: number };
  created_at?: string;
}

export type Host = components['schemas']['Host'];
export type Workspace = components['schemas']['Workspace'];
export type WorkspaceBinding = components['schemas']['WorkspaceBinding'];
export type Template = components['schemas']['Template'];

export interface AdminUser extends PortalUser {
  disabled: boolean;
  quota: NonNullable<PortalUser['quota']>;
  usage: NonNullable<PortalUser['usage']>;
  created_at: string;
}

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
