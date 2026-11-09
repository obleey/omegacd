import { conn } from './conn.svelte.ts';
import { goto } from '$app/navigation';

// JSON shapes mirror internal/domain/domain.go.
export type SyncStatus = 'Synced' | 'OutOfSync' | 'Unknown';
export type HealthStatus = 'Healthy' | 'Progressing' | 'Degraded' | 'Missing';
export type OpPhase = 'Running' | 'Succeeded' | 'Failed' | 'Waiting' | 'Superseded';

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
	pruneVolumes: boolean; // only with prune; the server turns it off without prune
	adopt: boolean;
	kind: 'compose' | 'apps'; // 'apps' = an App of Apps
	parent?: string; // the App of Apps that owns this Child Application
	notInGit: boolean; // a Child Application its parent no longer declares
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
	deleting?: 'containers' | 'volumes' | 'keep'; // being deleted, waiting for its Host's Agent (Q83)
	reconciledAt?: string;
	/** services no longer declared whose containers were stopped because Prune is off,
	 * and undeclared networks/volumes as "network <name>" / "volume <name>" (Docker names) */
	prunePending?: string[];
	/** App of Apps: the Hosts its Child Applications deploy to */
	hosts?: string[];
}

export interface ServiceDiff {
	/** set for an undeclared network or volume (service is then its Docker name); never drift */
	kind?: 'network' | 'volume';
	service: string;
	change: 'added' | 'removed' | 'changed';
	fields?: string[];
	/** a Child Application's Host, in an App of Apps' diff */
	host?: string;
}

export interface DiffView {
	fromSHA: string;
	toSHA: string;
	services: ServiceDiff[] | null;
	gitDiff: string;
	live: string; // compose of the deployed files; "" before the first Sync
	desired: string; // compose of the target Revision
	warnings?: string[]; // problems in the Desired compose files that do not stop a Sync
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
	resources: Resource[] | null;
	health: HealthStatus;
}

export interface Resource {
	kind: 'network' | 'volume' | 'env_file' | 'config' | 'secret' | 'bind';
	name: string;
	services?: string[];
	target?: string;
	secret?: boolean;
}

export interface ResourceDetail extends Resource {
	content?: string;
	masked?: boolean;
	info?: Record<string, string>;
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
	usedBy: string[]; // Applications whose Repository URL this credential serves
}

/** Whether OmegaCD can read a Repository; see RepoAccess in domain.go. */
export interface RepoAccess {
	access: 'public' | 'credential' | 'denied' | 'error';
	credentialPrefix?: string;
	defaultBranch?: string;
	message?: string;
}

export interface RegistryCredential {
	id: number;
	registry: string;
	username: string;
	hasPassword: boolean;
}

export interface HostStatus {
	name: string;
	connected: boolean;
	error?: string;
	serverVersion?: string;
	os?: string;
	containers: number;
}

export interface HostView {
	name: string;
	url: string; // "" for the built-in local Host
	workDir: string;
	hasSSHKey: boolean;
	hasTLS: boolean;
	status: HostStatus;
	apps: string[];
	keyRecipient: string; // current Host Key (the Master Key for local); "" if none
}

/** A Host Key's public half; private keys never leave the server. */
export interface HostKey {
	recipient: string;
	current: boolean;
	master?: boolean; // the Master Key: local's Host Key
	createdAt?: string;
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

/** The server could not be reached at all (restarting, network down) — not an API error. */
export class OfflineError extends ApiError {
	constructor() {
		super(0, 'OmegaCD is unreachable — reconnecting…');
	}
}

export async function api<T = unknown>(method: string, path: string, body?: unknown): Promise<T> {
	const headers: Record<string, string> = { Accept: 'application/json' };
	if (method !== 'GET') headers['X-Requested-With'] = 'omegacd';
	if (body !== undefined) headers['Content-Type'] = 'application/json';
	let res: Response;
	try {
		res = await fetch('/api' + path, {
			method,
			headers,
			credentials: 'same-origin',
			body: body === undefined ? undefined : JSON.stringify(body)
		});
	} catch {
		conn.down = true; // fetch only rejects on network failure
		throw new OfflineError();
	}
	conn.down = false;
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

export type PendingChild = { name: string; host?: string };
/** Child Applications an App of Apps declares in Git but hasn't created yet: its next Sync creates them. */
export const pendingChildren = (app: string) =>
	get<DiffView>(`/apps/${enc(app)}/diff`).then((d) =>
		(d.services ?? []).filter((s) => !s.kind && s.change === 'added').map((s): PendingChild => ({ name: s.service, host: s.host }))
	);

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
