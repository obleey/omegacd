// Add Host's commands (Q151, Q159, Q160): what enrolls a Host's Agent with its invite, before M6's installers add
// their own (install.sh, deploy/agent.yaml, systemd).

/** The image tag of this Control Plane's version: v0.2.0 → 0.2.0; a development build (no release tag) → latest. */
export function imageTag(version: string): string {
	const m = version.match(/^v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)$/);
	return m ? m[1] : 'latest';
}

/** The Agent's command line on the Host, as omegacd host add prints it. */
export function agentCommand(url: string, invite: string): string {
	return `OMEGACD_AGENT_CONTROL_PLANE=${url} OMEGACD_AGENT_INVITE=${invite} omegacd agent`;
}

/** The Agent in a container (Q159): the engine's socket, the work dir at the same path on both sides (Q21), its keys in
 * a named volume; root, as root in a container holding the socket is root on the Host already. */
export function dockerRun(url: string, invite: string, version: string): string {
	return [
		'docker run -d --name omegacd-agent --restart unless-stopped',
		'  -v /var/run/docker.sock:/var/run/docker.sock',
		'  -v /run/omegacd:/run/omegacd',
		'  -v omegacd-agent:/var/lib/omegacd-agent -e OMEGACD_AGENT_STATE_DIR=/var/lib/omegacd-agent',
		'  -e OMEGACD_AGENT_ALLOW_ROOT=true',
		`  -e OMEGACD_AGENT_CONTROL_PLANE=${url}`,
		`  -e OMEGACD_AGENT_INVITE=${invite}`,
		`  ghcr.io/obleey/omegacd:${imageTag(version)} agent`
	].join(' \\\n');
}

/** Why an Agent would refuse url (Q95, Q160): it connects over https only, except to its own machine; "" when fine. */
export function httpWarning(url: string): string {
	let u: URL;
	try {
		u = new URL(url);
	} catch {
		return 'Not an address: give the one Agents reach OmegaCD at, e.g. https://omegacd.example.com';
	}
	if (u.protocol === 'https:') return '';
	const loopback = u.hostname === 'localhost' || u.hostname === '[::1]' || /^127\./.test(u.hostname);
	if (u.protocol === 'http:' && loopback) return '';
	return 'Agents connect over https only: open OmegaCD over https (behind a reverse proxy), or the Agent refuses this address.';
}
