// Run with `npm test` (node --test; Node strips the TypeScript types).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parseLine, stripAnsi } from './ansi.ts';

const text = (l: ReturnType<typeof parseLine>) => l.segs.map((s) => s.text).join('');

test('splits off the Docker timestamp', () => {
	const l = parseLine('2026-11-04T19:12:01.123456789Z hello world');
	assert.equal(l.ts, '2026-11-04T19:12:01.123456789Z');
	assert.equal(text(l), 'hello world');
});

test('detects levels in plain, bracketed and JSON lines', () => {
	assert.equal(parseLine('ERROR could not connect').level, 'error');
	assert.equal(parseLine('[warn] disk almost full').level, 'warn');
	assert.equal(parseLine('time=… level=INFO msg="ok"').level, 'info');
	assert.equal(parseLine('{"level":"error","msg":"boom"}').level, 'error');
	assert.equal(parseLine('just a line').level, '');
	assert.equal(parseLine('terrorist').level, '', 'only whole words');
});

test('highlights the level word and key= names without changing the text', () => {
	const raw = 'level=WARN msg="slow" path=/api duration=3s';
	const l = parseLine(raw);
	assert.equal(text(l), raw);
	assert.ok(l.segs.some((s) => s.cls === 'lv-warn' && s.text === 'WARN'));
	assert.ok(l.segs.some((s) => s.cls === 'key' && s.text === 'path'));
});

test('renders ANSI colors and resets', () => {
	const l = parseLine('\x1b[31mred\x1b[0m plain \x1b[1;92mbold green\x1b[39m');
	assert.equal(text(l), 'red plain bold green');
	assert.match(l.segs[0].style ?? '', /--red/);
	assert.equal(l.segs[1].style, undefined);
	assert.match(l.segs[2].style ?? '', /--green.*font-weight: 700/);
});

test('256 and true colors', () => {
	assert.match(parseLine('\x1b[38;5;196mx').segs[0].style ?? '', /rgb\(255,0,0\)/);
	assert.match(parseLine('\x1b[38;2;1;2;3mx').segs[0].style ?? '', /rgb\(1,2,3\)/);
});

test('drops non-color escapes and keeps text intact', () => {
	assert.equal(stripAnsi('\x1b[2K\x1b[1Gdone \x1b[32m✓\x1b[0m'), 'done ✓');
	assert.equal(text(parseLine('\x1b]0;title\x07after')), 'after');
});
