// Add Host's commands (Q151, Q159, Q160): what enrolls a Host's Agent with its invite; M6's installers add theirs
// (deploy/agent.yaml here; install.sh and systemd to come).

/** The image tag of this Control Plane's version: v0.2.0 → 0.2.0; a development build (no release tag) → latest. */
export function imageTag(version: string): string {
	const m = version.match(/^v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)$/);
	return m ? m[1] : 'latest';
}

/** The Agent's command line on the Host, as omegacd host add prints it. */
export function agentCommand(url: string, invite: string): string {
	return `OMEGACD_AGENT_CONTROL_PLANE=${url} OMEGACD_AGENT_INVITE=${invite} omegacd agent`;
}

/** The Agent in a container with deploy/agent.yaml (M6-02, Q238): the file of this version, and a .env with the
 * address, the invite and the engine socket's group, which only the Host knows. Never root (Q236); a rootless engine
 * takes the lines the file's comment gives (Q240). */
export function composeAgent(url: string, invite: string, version: string): string {
	const tag = imageTag(version);
	const ref = tag === 'latest' ? 'main' : `v${tag}`;
	return [
		'mkdir -p omegacd-agent && cd omegacd-agent',
		`curl -fsSLO https://raw.githubusercontent.com/obleey/omegacd/${ref}/deploy/agent.yaml`,
		'cat > .env <<EOF',
		`OMEGACD_IMAGE=ghcr.io/obleey/omegacd:${tag}`,
		`OMEGACD_AGENT_CONTROL_PLANE=${url}`,
		`OMEGACD_AGENT_INVITE=${invite}`,
		'OMEGACD_DOCKER_GID=$(stat -c %g /var/run/docker.sock)',
		'EOF',
		'docker compose up -d'
	].join('\n');
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
