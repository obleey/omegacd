import { test } from 'node:test';
import assert from 'node:assert/strict';
import { bytes } from './size.ts';

test('sizes in powers of 1024', () => {
	assert.equal(bytes(0), '0 B');
	assert.equal(bytes(512), '512 B');
	assert.equal(bytes(2048), '2.0 KiB');
	assert.equal(bytes(1.5 * 1024 * 1024), '1.5 MiB');
	assert.equal(bytes(3.2 * 1024 ** 3), '3.2 GiB');
	assert.equal(bytes(2 * 1024 ** 5), '2048.0 TiB'); // no bigger unit
});
