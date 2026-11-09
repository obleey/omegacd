import { errMsg, OfflineError } from './api.ts';

export type Toast = { id: number; tone: 'green' | 'yellow' | 'red' | 'blue'; title: string; body?: string; href?: string };

export const toasts = $state<Toast[]>([]);
let next = 1;

export function dismiss(id: number) {
	const i = toasts.findIndex((t) => t.id === id);
	if (i >= 0) toasts.splice(i, 1);
}

export function toast(tone: Toast['tone'], title: string, body?: string, href?: string) {
	const id = next++;
	toasts.push({ id, tone, title, body, href });
	if (toasts.length > 5) toasts.shift();
	setTimeout(() => dismiss(id), tone === 'red' ? 9000 : 5000);
}

/** Toast an API error; returns its message for callers that also show it inline. */
export function toastError(e: unknown, title = 'Request failed') {
	const msg = errMsg(e);
	// unreachable server: the "Reconnecting…" banner says it once, instead of a toast per request
	if (e instanceof OfflineError) return msg;
	toast('red', title, msg);
	return msg;
}
