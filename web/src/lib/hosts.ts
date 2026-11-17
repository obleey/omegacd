// Host cards (M5-02, Q37): what a Host's card says about it and its Agent.
import type { AgentInfo, HostView } from './api.ts';

/** Where the Host stands in enrollment: invited, enrolled awaiting approval, approved; locked or refused over it. */
export function hostState(h: HostView): 'Invited' | 'To approve' | 'Approved' | 'Locked' | 'Refused' {
	if (!h.fingerprint) return 'Invited';
	if (h.locked?.length) return 'Locked';
	if (h.agent?.refused) return 'Refused';
	return h.approved ? 'Approved' : 'To approve';
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
