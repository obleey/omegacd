import { test } from 'node:test';
import assert from 'node:assert/strict';
import type { AppView, HostView } from './api.ts';
import { appBadges, hostBadges, triggerLabel } from './badges.ts';

const host = (h: Partial<HostView>): HostView =>
	({ name: 'edge', createdAt: '', approved: true, fingerprint: 'SHA256:x', status: { name: 'edge', connected: true }, apps: [], ...h }) as HostView;
const agent = { version: 'v0.2.0', protocol: 1, seenAt: '2026-11-17T10:00:00Z', engine: 'podman', engineVersion: '5.4.2', rootless: true };
const app = (a: Partial<AppView>): AppView => ({ name: 'web', kind: 'compose', host: 'edge', rolledBack: false, ...a }) as AppView;
const texts = (bs: { text: string }[]) => bs.map((b) => b.text);

test('a connected Host shows its engine; the version and rootless in the tooltip', () => {
	const bs = hostBadges(host({ agent }));
	assert.deepEqual(texts(bs), ['Podman']);
	assert.equal(bs[0].title, 'Engine: Podman 5.4.2, rootless');
	assert.equal(hostBadges(host({ agent: { ...agent, engine: 'docker', rootless: false } }))[0].title, 'Engine: Docker 5.4.2');
});

test('offline (only once enrolled) and outdated, saying what to do', () => {
	const off = hostBadges(host({ agent: { ...agent, outdated: true }, status: { name: 'edge', connected: false } }));
	assert.deepEqual(texts(off), ['Podman', 'Host offline', 'Agent outdated']);
	assert.match(off[1].title, /edge is offline: its Applications keep running/);
	assert.match(off[2].title, /upgrade it on edge/);
	assert.deepEqual(hostBadges(host({ fingerprint: undefined, status: { name: 'edge', connected: false } })), []); // only invited
	assert.deepEqual(hostBadges(undefined), []);
});

test("an Application adds rolled back on the Host and a queued Sync to its Host's", () => {
	const h = host({ agent });
	const rolled = app({ rolledBack: true, lastOperation: { trigger: 'host-rollback', phase: 'Succeeded' } as AppView['lastOperation'] });
	assert.deepEqual(texts(appBadges(rolled, h)), ['Rolled back on the Host']); // the engine is text by the Host's name (Q181)
	const ui = app({ rolledBack: true, lastOperation: { trigger: 'rollback', phase: 'Succeeded' } as AppView['lastOperation'] });
	assert.deepEqual(texts(appBadges(ui, h)), []); // a Rollback through OmegaCD isn't the Host's
	assert.deepEqual(texts(appBadges(app({}), host({ agent, status: { name: 'edge', connected: false } }))), ['Host offline']);
	const queued = app({ lastOperation: { trigger: 'manual', phase: 'Waiting' } as AppView['lastOperation'] });
	assert.deepEqual(texts(appBadges(queued, undefined)), ['Sync queued']);
	assert.deepEqual(appBadges(app({ kind: 'apps' }), h), []);
});

test("the history names a rollback on the Host and the App of Apps' Syncs; the rest as they are", () => {
	assert.equal(triggerLabel('host-rollback'), 'rollback on the Host');
	assert.equal(triggerLabel('app-of-apps'), 'App of Apps');
	assert.equal(triggerLabel('manual'), 'manual');
});
