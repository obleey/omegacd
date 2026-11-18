// Host cards (M5-02, Q37): what a Host's card says about it and its Agent.
import type { AgentInfo, HostView } from './api.ts';
import type { Tone } from './status.ts';

/** Where the Host stands in enrollment: invited, enrolled awaiting approval, approved; locked or refused over it. */
export function hostState(h: HostView): 'Invited' | 'To approve' | 'Approved' | 'Locked' | 'Refused' {
	if (!h.fingerprint) return 'Invited';
	if (h.locked?.length) return 'Locked';
	if (h.agent?.refused) return 'Refused';
	return h.approved ? 'Approved' : 'To approve';
}

/** One color summarising a Host, worst signal first (its card's status bar), as an Application's tile has. */
export function hostTone(h: HostView): Tone {
	const state = hostState(h);
	if (state === 'Locked' || state === 'Refused') return 'red';
	if (state === 'Invited') return 'overlay';
	if (state === 'To approve') return 'yellow';
	if (!h.status.connected) return 'red';
	return h.agent?.outdated ? 'yellow' : 'green';
}

/** Online, or when its Agent was last connected: undefined for one never connected. */
export function lastSeen(h: HostView): string | undefined {
	if (h.status.connected) return 'online';
	return h.agent?.leftAt ?? h.agent?.seenAt;
}

/** "Docker 29.1 · rootless": the engine its Agent deploys with. */
export function engineLabel(a?: AgentInfo): string {
	if (!a?.engine) return '—';
	const name = a.engine === 'podman' ? 'Podman' : 'Docker';
	return [`${name} ${a.engineVersion ?? ''}`.trim(), a.rootless ? 'rootless' : ''].filter(Boolean).join(' · ');
}

/** The allowlist editor's text as the API takes it: one address or CIDR per line (commas and spaces too). */
export const parseAllowlist = (text: string) => text.split(/[\s,]+/).filter(Boolean);

/** Why a Host can't be renamed (Q53): Git names it in its Applications. */
export function renameBlocked(h: HostView): string | undefined {
	if (h.apps.length) return `Used by ${h.apps.join(', ')}: rename it in their definitions after moving them`;
}

/** A Host's Applications grouped by the App of Apps that declares them, those groups first, then the standalone ones
 * (no parent): "platform: blog counter …", "docs echo …". An App of Apps deploys nothing itself: it heads its group,
 * it isn't listed as running on the Host. */
export function groupByParent(apps: string[], parentOf: (app: string) => string | undefined): { parent?: string; apps: string[] }[] {
	const groups = new Map<string | undefined, string[]>();
	for (const a of apps) {
		const p = parentOf(a);
		groups.set(p, [...(groups.get(p) ?? []), a]);
	}
	const parents = [...groups.keys()].filter((p): p is string => !!p).sort();
	return [...parents.map((p) => ({ parent: p, apps: groups.get(p)! })), ...(groups.has(undefined) ? [{ apps: groups.get(undefined)! }] : [])];
}
