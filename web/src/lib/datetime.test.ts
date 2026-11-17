import { test } from 'node:test';
import assert from 'node:assert/strict';
import { formatDate } from './datetime.ts';

const evening = new Date(2026, 10, 17, 18, 42); // local time, as shown
const morning = new Date(2026, 0, 5, 0, 7);

test('European: day first, 24-hour', () => {
	assert.equal(formatDate(evening, 'eu'), '17.11.2026 18:42');
	assert.equal(formatDate(morning, 'eu'), '05.01.2026 00:07');
	assert.equal(formatDate(evening, 'eu', 'time'), '18:42');
});

test('US: month first, 12-hour', () => {
	assert.equal(formatDate(evening, 'us'), '11/17/2026 6:42 PM');
	assert.equal(formatDate(morning, 'us'), '1/5/2026 12:07 AM');
	assert.equal(formatDate(new Date(2026, 0, 5, 12, 0), 'us', 'time'), '12:00 PM');
});

test("the browser's own format, from an API timestamp too", () => {
	assert.equal(formatDate(evening, 'browser'), evening.toLocaleString());
	assert.equal(formatDate(evening.toISOString(), 'eu'), '17.11.2026 18:42');
});
