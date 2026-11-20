import { test } from 'node:test';
import assert from 'node:assert/strict';
import type { HostView } from './api.ts';
import { engineLabel, groupByParent, workDirLabel, workDirProblem, hostState, hostTone, lastSeen, parseAllowlist, renameBlocked } from './hosts.ts';

const host = (h: Partial<HostView>): HostView =>
	({ name: 'web', createdAt: '', approved: false, status: { name: 'web', connected: false }, apps: [], ...h }) as HostView;
const agent = { version: 'v0.2.0', protocol: 1, seenAt: '2026-11-17T10:00:00Z' };

test('a Host stands invited, to approve, approved, locked or refused', () => {
	assert.equal(hostState(host({})), 'Invited');
	assert.equal(hostState(host({ fingerprint: 'SHA256:x' })), 'To approve');
	assert.equal(hostState(host({ fingerprint: 'SHA256:x', approved: true })), 'Approved');
	assert.equal(hostState(host({ fingerprint: 'SHA256:x', approved: true, locked: [{ address: 'a', since: '' }] })), 'Locked');
	assert.equal(hostState(host({ fingerprint: 'SHA256:x', approved: true, agent: { ...agent, refused: 'too old' } })), 'Refused');
});

test("a card's color: green connected, red offline or locked, yellow to act on, grey only invited", () => {
	const on = { name: 'web', connected: true };
	const enrolled = { fingerprint: 'SHA256:x', approved: true };
	assert.equal(hostTone(host({ ...enrolled, status: on, agent })), 'green');
	assert.equal(hostTone(host({ ...enrolled, status: on, agent: { ...agent, outdated: true } })), 'yellow');
	assert.equal(hostTone(host({ ...enrolled })), 'red');
	assert.equal(hostTone(host({ ...enrolled, status: on, locked: [{ address: 'a', since: '' }] })), 'red');
	assert.equal(hostTone(host({ ...enrolled, agent: { ...agent, refused: 'too old' } })), 'red');
	assert.equal(hostTone(host({ fingerprint: 'SHA256:x', status: on })), 'yellow');
	assert.equal(hostTone(host({})), 'overlay');
});

test('online, or last seen when it left, or never', () => {
	assert.equal(lastSeen(host({ status: { name: 'web', connected: true } })), 'online');
	assert.equal(lastSeen(host({ agent: { ...agent, leftAt: '2026-11-17T11:00:00Z' } })), '2026-11-17T11:00:00Z');
	assert.equal(lastSeen(host({ agent })), '2026-11-17T10:00:00Z'); // refused before it was served
	assert.equal(lastSeen(host({})), undefined);
});

test('the engine, with rootless', () => {
	assert.equal(engineLabel({ ...agent, engine: 'podman', engineVersion: '5.4.2', rootless: true }), 'Podman 5.4.2 · rootless');
	assert.equal(engineLabel({ ...agent, engine: 'docker', engineVersion: '29.1.0' }), 'Docker 29.1.0');
	assert.equal(engineLabel(agent), '—');
	assert.equal(engineLabel(undefined), '—');
});

test('the allowlist editor takes lines, commas or spaces; empty is any', () => {
	assert.deepEqual(parseAllowlist('10.0.0.0/8\n 203.0.113.7, 2001:db8::/32 \n\n'), ['10.0.0.0/8', '203.0.113.7', '2001:db8::/32']);
	assert.deepEqual(parseAllowlist('  \n'), []);
});

test('renamed only while no Application uses it', () => {
	assert.equal(renameBlocked(host({})), undefined);
	assert.match(renameBlocked(host({ apps: ['shop', 'blog'] }))!, /shop, blog/);
});

test("a Host's Applications grouped by their App of Apps, standalone ones last", () => {
	const parent: Record<string, string> = { blog: 'platform', shop: 'platform', api: 'backend' };
	assert.deepEqual(groupByParent(['blog', 'docs', 'shop', 'api', 'echo'], (a) => parent[a]), [
		{ parent: 'backend', apps: ['api'] },
		{ parent: 'platform', apps: ['blog', 'shop'] },
		{ apps: ['docs', 'echo'] }
	]);
	assert.deepEqual(groupByParent(['docs'], () => undefined), [{ apps: ['docs'] }]);
	assert.deepEqual(groupByParent([], () => undefined), []);
});

test("an Application's work dir against the roots its Host's Agent reported", () => {
	const a = { ...agent, workDir: '/run/omegacd', workDirAllow: ['/srv/apps'] };
	assert.equal(workDirProblem('', 'web', 'edge', a), ''); // the Host's own
	assert.equal(workDirProblem('/srv/apps/shop/', 'web', 'edge', a), '');
	assert.equal(workDirProblem('/run/omegacd', 'web', 'edge', a), '');
	assert.match(workDirProblem('/srv/apps2', 'web', 'edge', a), /not under a root Host edge's Agent allows \(its own \/run\/omegacd, and OMEGACD_AGENT_WORKDIR_ALLOW: \/srv\/apps\)/);
	assert.equal(workDirProblem('srv/apps', 'web', 'edge', a), 'An absolute path, not /.');
	assert.equal(workDirProblem('/', 'web', 'edge', a), 'An absolute path, not /.');
	assert.equal(workDirProblem('/srv/../etc', 'web', 'edge', a), 'An absolute path, not /.');
	assert.equal(workDirProblem('/etc', 'web', 'edge', agent), ''); // not reported yet: the Agent decides
	assert.match(workDirProblem('/etc', '', 'edge', { ...agent, workDir: '/run/omegacd' }), /this Application.*OMEGACD_AGENT_WORKDIR_ALLOW: none/);
});

test("a Host's work dir and its allowed roots", () => {
	assert.equal(workDirLabel({ ...agent, workDir: '/run/omegacd', workDirAllow: ['/srv/apps', '/data'] }), '/run/omegacd (also /srv/apps, /data)');
	assert.equal(workDirLabel({ ...agent, workDir: '/run/omegacd' }), '/run/omegacd');
	assert.equal(workDirLabel(agent), '—');
	assert.equal(workDirLabel(undefined), '—');
});
