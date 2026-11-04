import { goto } from '$app/navigation';

// JSON shapes mirror internal/domain/domain.go.
export type SyncStatus = 'Synced' | 'OutOfSync' | 'Unknown';
export type HealthStatus = 'Healthy' | 'Progressing' | 'Degraded' | 'Missing';
export type OpPhase = 'Running' | 'Succeeded' | 'Failed';

export interface Application {
	name: string;
	repoURL: string;
	path: string;
	targetRevision: string;
	composeFiles: string[] | null;
	profiles: string[] | null;
	host?: string;
	autoSync: boolean;
	selfHeal: boolean;
	prune: boolean;
	adopt: boolean;
	createdAt?: string;
}

export interface SyncOperation {
	id: number;
	app: string;
	revision: string;
	treeHash: string;
	phase: OpPhase;
	message: string;
	trigger: string;
	startedAt: string;
	finishedAt?: string;
}

export interface AppView extends Application {
	syncStatus: SyncStatus;
	syncMessage?: string;
	health: HealthStatus;
	targetSHA?: string;
	deployedSHA?: string;
	lastOperation?: SyncOperation;
	rolledBack: boolean;
	reconciledAt?: string;
}

export interface ServiceDiff {
	service: string;
	change: 'added' | 'removed' | 'changed';
	fields?: string[];
}

export interface DiffView {
	fromSHA: string;
	toSHA: string;
	services: ServiceDiff[] | null;
	gitDiff: string;
}

export interface Container {
	id: string;
	name: string;
	service: string;
	image: string;
	state: string;
	status: string;
	exitCode: number;
	health: HealthStatus;
}

export interface LiveState {
	services: string[] | null;
	containers: Container[] | null;
	resources: { kind: string; name: string }[] | null;
	health: HealthStatus;
}

export interface ProgressEvent {
	opID: number;
	resource: string;
	status: string;
	text?: string;
}

export interface RepoCredential {
	id: number;
	urlPrefix: string;
	username?: string;
	hasPassword: boolean;
	hasSSHKey: boolean;
}

export interface RegistryCredential {
	id: number;
	registry: string;
	username: string;
	hasPassword: boolean;
}

export interface APIToken {
	id: number;
	name: string;
	createdAt: string;
	lastUsed?: string;
}

export class ApiError extends Error {
	constructor(
		public status: number,
		message: string
	) {
		super(message);
	}
}

export async function api<T = unknown>(method: string, path: string, body?: unknown): Promise<T> {
	const headers: Record<string, string> = { Accept: 'application/json' };
	if (method !== 'GET') headers['X-Requested-With'] = 'dockercd';
	if (body !== undefined) headers['Content-Type'] = 'application/json';
	const res = await fetch('/api' + path, {
		method,
		headers,
		credentials: 'same-origin',
		body: body === undefined ? undefined : JSON.stringify(body)
	});
	if (res.status === 401 && path !== '/auth/login') {
		await goto('/login');
		throw new ApiError(401, 'not logged in');
	}
	if (!res.ok) {
		let msg = res.statusText;
		try {
			msg = (await res.json()).error ?? msg;
		} catch {
			/* non-JSON error body */
		}
		throw new ApiError(res.status, msg);
	}
	if (res.status === 204) return undefined as T;
	const type = res.headers.get('Content-Type') ?? '';
	return (type.includes('json') ? res.json() : res.text()) as Promise<T>;
}

export const get = <T>(path: string) => api<T>('GET', path);

export function errMsg(e: unknown): string {
	return e instanceof Error ? e.message : String(e);
}

export const short = (sha?: string) => (sha ? sha.slice(0, 7) : '—');

export function ago(ts?: string): string {
	if (!ts) return '—';
	const s = Math.round((Date.now() - new Date(ts).getTime()) / 1000);
	if (s < 60) return 'just now';
	if (s < 3600) return `${Math.floor(s / 60)}m ago`;
	if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
	return `${Math.floor(s / 86400)}d ago`;
}

export const enc = encodeURIComponent;
