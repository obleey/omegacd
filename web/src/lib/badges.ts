// Badges about an Application's Host (M5-03, Q39): its engine, offline, Agent outdated; and the Application's own
// rolled back on the Host and queued Sync. Each with a tooltip saying what it means and what to do.
import type { AppView, HostView } from './api.ts';

export type HostBadge = { status: string; text: string; title: string };

/** What a Host shows wherever it's named: its engine; offline; its Agent outdated. */
export function hostBadges(h?: HostView): HostBadge[] {
	if (!h) return [];
	const out: HostBadge[] = [];
	const a = h.agent;
	if (a?.engine) {
		const name = a.engine === 'podman' ? 'Podman' : 'Docker';
		out.push({ status: name, text: name, title: `Engine: ${name} ${a.engineVersion ?? ''}${a.rootless ? ', rootless' : ''}`.trim() });
	}
	if (h.fingerprint && !h.status.connected)
		out.push({ status: 'Host offline', text: 'Host offline', title: `Host ${h.name} is offline: its Applications keep running; Syncs wait for it` });
	if (a?.outdated) out.push({ status: 'Outdated', text: 'Agent outdated', title: `Agent ${a.version} is older than OmegaCD: upgrade it on ${h.name}` });
	return out;
}

/** What an Application shows about its Host and its own state there, only what needs attention: its engine is text by
 * the Host's name (Q181). An App of Apps deploys nothing itself: none. */
export function appBadges(app: AppView, host?: HostView): HostBadge[] {
	if (app.kind === 'apps') return [];
	const out = hostBadges(host).filter((b) => b.status !== 'Docker' && b.status !== 'Podman');
	const op = app.lastOperation;
	if (app.rolledBack && op?.trigger === 'host-rollback')
		out.push({ status: 'Rolled back on Host', text: 'Rolled back on the Host', title: 'Rolled back on the Host with omegacd agent rollback: Auto-Sync is off until a Sync' });
	if (op?.phase === 'Waiting')
		out.push({ status: 'Queued', text: 'Sync queued', title: `A Sync waits for Host ${app.host}: it runs when its Agent connects` });
	return out;
}

/** What started a Sync Operation, as the history says it. */
export const triggerLabel = (t: string) => ({ 'host-rollback': 'rollback on the Host', 'app-of-apps': 'App of Apps' })[t] ?? t;
