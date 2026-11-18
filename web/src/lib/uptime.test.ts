import { test } from 'node:test';
import assert from 'node:assert/strict';
import type { Container } from './api.ts';
import { duration, uptime } from './uptime.ts';

const now = Date.parse('2026-11-17T20:00:00Z');
const c = (x: Partial<Container>): Container =>
	({ id: 'a', name: 'web-1', service: 'web', image: 'nginx', state: 'running', status: 'Up 3 seconds', exitCode: 0, health: 'Healthy', ...x }) as Container;

test('durations read as people say them', () => {
	assert.equal(duration(45_000), '45 s');
	assert.equal(duration(3 * 60_000 + 20_000), '3 min');
	assert.equal(duration((2 * 60 + 14) * 60_000), '2 h 14 min');
	assert.equal(duration(2 * 3_600_000), '2 h');
	assert.equal(duration((3 * 24 + 4) * 3_600_000), '3 d 4 h');
	assert.equal(duration(-5), '0 s'); // a clock a little ahead
});

test('up since its start, exited since it stopped, counted from now', () => {
	assert.equal(uptime(c({ startedAt: '2026-11-17T17:46:00Z' }), now), 'Up 2 h 14 min');
	assert.equal(uptime(c({ state: 'exited', exitCode: 1, finishedAt: '2026-11-17T19:55:00Z', startedAt: '2026-11-17T10:00:00Z' }), now), 'Exited (1) 5 min ago');
});

test("an older Agent's container without times shows the engine's status line", () => {
	assert.equal(uptime(c({}), now), 'Up 3 seconds');
	assert.equal(uptime(c({ state: 'restarting', startedAt: '2026-11-17T19:00:00Z', status: 'Restarting (1) 2 seconds ago' }), now), 'Restarting (1) 2 seconds ago');
});
