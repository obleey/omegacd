// Shared live state fed by one EventSource on /api/events.
import { get, type AppView, type ProgressEvent, type SyncOperation } from './api';

export const live = $state({
	me: null as { username: string; mustChangePassword: boolean } | null,
	apps: {} as Record<string, AppView>,
	loaded: false,
	connected: false,
	/** newest op seen per Application, from "op" events */
	ops: {} as Record<string, SyncOperation>,
	/** progress lines per op ID, from "progress" events */
	progress: {} as Record<number, ProgressEvent[]>
});

export async function loadApps() {
	const list = await get<AppView[]>('/apps');
	live.apps = Object.fromEntries((list ?? []).map((a) => [a.name, a]));
	live.loaded = true;
}

export function putApp(a: AppView) {
	live.apps[a.name] = a;
}

let es: EventSource | null = null;

// Data is a domain.Event ({type, app, data}); tolerate a bare payload too.
function unwrap(raw: string): { app: string; data: unknown } {
	const v = JSON.parse(raw);
	if (v && typeof v === 'object' && 'type' in v && 'data' in v) return { app: v.app, data: v.data };
	return { app: '', data: v };
}

export function connectEvents() {
	if (es) return;
	es = new EventSource('/api/events', { withCredentials: true });
	es.onopen = () => (live.connected = true);
	es.onerror = () => {
		live.connected = false;
		if (es?.readyState === EventSource.CLOSED) {
			// HTTP error (e.g. 401): get() redirects to /login on 401; otherwise retry.
			es = null;
			get('/auth/me')
				.then(() => setTimeout(connectEvents, 3000))
				.catch(() => {});
		}
	};
	es.addEventListener('app', (e) => {
		const a = unwrap(e.data).data as AppView;
		live.apps[a.name] = a;
	});
	es.addEventListener('app-deleted', (e) => {
		const { app, data } = unwrap(e.data);
		delete live.apps[typeof data === 'string' ? data : app];
	});
	es.addEventListener('op', (e) => {
		const op = unwrap(e.data).data as SyncOperation;
		live.ops[op.app] = op;
		const a = live.apps[op.app];
		if (a) a.lastOperation = op;
	});
	es.addEventListener('progress', (e) => {
		const p = unwrap(e.data).data as ProgressEvent;
		const list = (live.progress[p.opID] ??= []);
		// keep one line per resource, latest status wins
		const i = list.findIndex((x) => x.resource === p.resource);
		if (i >= 0) list[i] = p;
		else list.push(p);
	});
}

export function disconnectEvents() {
	es?.close();
	es = null;
	live.connected = false;
}
