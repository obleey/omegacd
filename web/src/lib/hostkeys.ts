// Host Keys in the UI: shortened recipients and ready-to-paste .sops.yaml creation rules.

export const shortKey = (r: string) => (r.length > 20 ? `${r.slice(0, 10)}…${r.slice(-6)}` : r);

/** One creation_rules entry per Host; the path_regex is a placeholder (host names need no escaping: [a-z0-9_-]). */
function rule(host: string, recipient: string): string {
	if (!recipient) return `  # Host ${host} has no Host Key yet — generate or import one in Settings → Hosts\n`;
	const who = host === 'local' ? 'local Host (the Master Key)' : `Host ${host}`;
	return `  # ${who}; the path is up to you: match the files of the Applications deployed to it\n` +
		`  - path_regex: apps/${host}/.*\\.sops\\.\n    age: ${recipient}\n`;
}

export function sopsYaml(hosts: { name: string; recipient: string }[]): string {
	return 'creation_rules:\n' + hosts.map((h) => rule(h.name, h.recipient)).join('');
}
