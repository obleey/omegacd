// Host Keys in the UI: shortened recipients and ready-to-paste .sops.yaml creation rules.
import type { EncryptionKey } from './api.ts';

export const shortKey = (r: string) => (r.length > 20 ? `${r.slice(0, 10)}…${r.slice(-6)}` : r);

/** The people's keys covering host (Q190): its secrets are encrypted for them besides its Host Key. */
export const peopleFor = (host: string, keys: EncryptionKey[]) =>
	keys.filter((k) => k.allHosts || k.hosts.includes(host)).map((k) => k.recipient);

/** One creation_rules entry per Host, marked as OmegaCD's so omegacd secrets rekey keeps it current (Q288); the
 * path_regex is a placeholder (host names need no escaping: [a-z0-9_-]). */
function rule(host: string, recipient: string, people: string[]): string {
	if (!recipient) return `  # Host ${host} has no Host Key yet: its Agent makes one when it enrolls\n`;
	return `  # Host ${host}; the path is up to you: match the files of the Applications deployed to it\n` +
		`  # omegacd: Host ${host}\n` +
		`  - path_regex: apps/${host}/.*\\.sops\\.\n    age: ${[recipient, ...people].join(',')}\n`;
}

export function sopsYaml(hosts: { name: string; recipient: string }[], keys: EncryptionKey[]): string {
	return 'creation_rules:\n' + hosts.map((h) => rule(h.name, h.recipient, peopleFor(h.name, keys))).join('');
}
