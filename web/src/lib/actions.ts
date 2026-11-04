// Application actions shared by tiles and list rows; failures surface as toasts.
import { api, enc, short, type AppView } from './api.ts';
import { live, note, putApp } from './live.svelte.ts';
import { toastError } from './toast.svelte.ts';

export const syncApp = (name: string) =>
	api('POST', `/apps/${enc(name)}/sync`).catch((e) => toastError(e, `Sync of ${name} failed to start`));

// A refresh usually takes a few milliseconds; keep the spinner up long enough to be seen.
const atLeast = <T>(ms: number, p: Promise<T>) => Promise.all([p, new Promise((r) => setTimeout(r, ms))]).then(([v]) => v);

export const refreshApp = (name: string) => {
	const before = live.apps[name];
	return atLeast(600, api<AppView>('POST', `/apps/${enc(name)}/refresh`))
		.then((after) => {
			putApp(after);
			// say what the refresh found, so a quiet refresh still gives an answer
			if (before?.targetSHA && after.targetSHA && after.targetSHA !== before.targetSHA)
				note(name, `New revision ${short(after.targetSHA)}`, 'blue');
			else if (before && after.syncStatus !== before.syncStatus) note(name, `Now ${after.syncStatus}`, after.syncStatus === 'Synced' ? 'green' : 'yellow');
			else note(name, after.syncStatus === 'Synced' ? 'Up to date' : `Still ${after.syncStatus}`, after.syncStatus === 'Synced' ? 'green' : 'yellow');
		})
		.catch((e) => toastError(e, `Refresh of ${name} failed`));
};
