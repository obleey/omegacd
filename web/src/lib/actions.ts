// Application actions shared by tiles and list rows; failures surface as toasts.
import { api, enc, type AppView } from './api.ts';
import { putApp } from './live.svelte.ts';
import { toastError } from './toast.svelte.ts';

export const syncApp = (name: string) =>
	api('POST', `/apps/${enc(name)}/sync`).catch((e) => toastError(e, `Sync of ${name} failed to start`));

export const refreshApp = (name: string) =>
	api<AppView>('POST', `/apps/${enc(name)}/refresh`)
		.then(putApp)
		.catch((e) => toastError(e, `Refresh of ${name} failed`));
