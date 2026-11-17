import { test } from 'node:test';
import assert from 'node:assert/strict';
import { agentCommand, dockerRun, httpWarning, imageTag } from './addhost.ts';

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
	const run = dockerRun('https://cd.example.com', 'omegacd-invite-x', 'v0.2.0');
	for (const want of [
		'-v /run/omegacd:/run/omegacd',
		'-v /var/run/docker.sock:/var/run/docker.sock',
		'-e OMEGACD_AGENT_CONTROL_PLANE=https://cd.example.com',
		'-e OMEGACD_AGENT_INVITE=omegacd-invite-x',
		'ghcr.io/obleey/omegacd:0.2.0 agent'
	])
		assert.ok(run.includes(want), want);
	assert.ok(run.split('\n').slice(0, -1).every((l) => l.endsWith(' \\')), 'one command, continued on every line');
});

test('Agents connect over https, or plain http to their own machine', () => {
	assert.equal(httpWarning('https://cd.example.com'), '');
	assert.equal(httpWarning('http://localhost:8080'), '');
	assert.equal(httpWarning('http://127.0.0.1:8080'), '');
	assert.equal(httpWarning('http://[::1]:8080'), '');
	assert.match(httpWarning('http://cd.example.com'), /https only/);
	assert.match(httpWarning('not a url'), /Not an address/);
});
