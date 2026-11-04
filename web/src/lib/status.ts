import { prefersReducedMotion } from 'svelte/motion';
import type { AppView } from './api.ts';
import type { IconName } from './Icon.svelte';

export type Tone = 'green' | 'yellow' | 'red' | 'blue' | 'overlay';

// Icon, color and motion per status value. Color is never the only signal: every badge keeps its text.
export const statusMeta: Record<string, [Tone, IconName, boolean?]> = {
	// Sync Status ("Syncing" is shown while a Sync Operation runs)
	Synced: ['green', 'check-circle'],
	OutOfSync: ['yellow', 'arrow-circle'],
	Unknown: ['overlay', 'question'],
	Syncing: ['blue', 'sync', true],
	// Health Status
	Healthy: ['green', 'heart'],
	Progressing: ['blue', 'notch', true],
	Degraded: ['red', 'heart-broken'],
	Missing: ['yellow', 'ghost'],
	// Sync Operation phase
	Succeeded: ['green', 'check-circle'],
	Running: ['blue', 'notch', true],
	Failed: ['red', 'x-circle'],
	// Host connection
	Connected: ['green', 'check-circle'],
	Disconnected: ['red', 'x-circle'],
	// container state
	running: ['green', 'check-circle'],
	restarting: ['yellow', 'notch', true],
	created: ['yellow', 'stop-circle'],
	paused: ['yellow', 'stop-circle'],
	dead: ['red', 'x-circle'],
	exited: ['overlay', 'stop-circle'],
	removing: ['overlay', 'notch', true]
};
export const meta = (s?: string) => statusMeta[s ?? ''] ?? statusMeta.Unknown;

export const isSyncing = (a?: AppView) => a?.lastOperation?.phase === 'Running';
/** Sync Status as shown: "Syncing" while a Sync Operation runs. */
export const shownSync = (a: AppView) => (isSyncing(a) ? 'Syncing' : a.syncStatus || 'Unknown');

/** One color summarising an Application, worst signal first (tile status bar). */
export function appTone(a: AppView): Tone {
	if (isSyncing(a)) return 'blue';
	if (a.health === 'Degraded' || a.lastOperation?.phase === 'Failed') return 'red';
	if (a.syncStatus === 'OutOfSync' || a.health === 'Missing') return 'yellow';
	if (a.health === 'Progressing') return 'blue';
	if (a.syncStatus === 'Unknown') return 'overlay';
	return 'green';
}

/** Transition duration that collapses to 0 when the user prefers reduced motion. */
export const ms = (n: number) => (prefersReducedMotion.current ? 0 : n);
