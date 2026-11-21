import { test } from 'node:test';
import assert from 'node:assert/strict';
import { agentCommand, composeAgent, httpWarning, imageTag } from './addhost.ts';

test('the image is this version, a development build the latest release', () => {
	assert.equal(imageTag('v0.2.0'), '0.2.0');
	assert.equal(imageTag('v0.2.0-rc.1'), '0.2.0-rc.1');
	assert.equal(imageTag('(devel)'), 'latest');
	assert.equal(imageTag('v0.2.0-3-gabc1234-dirty'), 'latest');
	assert.equal(imageTag(''), 'latest');
});

test('the commands carry the address and the invite', () => {
	assert.equal(
		agentCommand('https://cd.example.com', 'omegacd-invite-x'),
		'OMEGACD_AGENT_CONTROL_PLANE=https://cd.example.com OMEGACD_AGENT_INVITE=omegacd-invite-x omegacd agent'
	);
	const c = composeAgent('https://cd.example.com', 'omegacd-invite-x', 'v0.2.0');
	for (const want of [
		'curl -fsSLO https://raw.githubusercontent.com/obleey/omegacd/v0.2.0/deploy/agent.yaml',
		'OMEGACD_IMAGE=ghcr.io/obleey/omegacd:0.2.0',
		'OMEGACD_AGENT_CONTROL_PLANE=https://cd.example.com',
		'OMEGACD_AGENT_INVITE=omegacd-invite-x',
		'OMEGACD_DOCKER_GID=$(stat -c %g /var/run/docker.sock)',
		'docker compose up -d'
	])
		assert.ok(c.includes(want), want);
	assert.ok(!c.includes('ALLOW_ROOT'), 'never root (Q236)');
	assert.ok(composeAgent('http://127.0.0.1:8080', 'i', '(devel)').includes('/obleey/omegacd/main/deploy/agent.yaml'), 'a development build: main');
});

test('Agents connect over https, or plain http to their own machine', () => {
	assert.equal(httpWarning('https://cd.example.com'), '');
	assert.equal(httpWarning('http://localhost:8080'), '');
	assert.equal(httpWarning('http://127.0.0.1:8080'), '');
	assert.equal(httpWarning('http://[::1]:8080'), '');
	assert.match(httpWarning('http://cd.example.com'), /https only/);
	assert.match(httpWarning('not a url'), /Not an address/);
});
