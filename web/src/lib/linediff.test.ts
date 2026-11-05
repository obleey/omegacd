import { test } from 'node:test';
import assert from 'node:assert/strict';
import { lineDiff } from './linediff.ts';

const render = (a: string, b: string) =>
	lineDiff(a, b)
		.map((l) => l.op + l.text)
		.join('\n');

test('identical texts have no changes', () => {
	assert.ok(lineDiff('a\nb\n', 'a\nb\n').every((l) => l.op === ' '));
});

test('a changed line is a removal followed by an addition, context kept', () => {
	assert.equal(
		render('name: web\nimage: nginx:1.26\nports: []\n', 'name: web\nimage: nginx:1.27\nports: []\n'),
		' name: web\n-image: nginx:1.26\n+image: nginx:1.27\n ports: []'
	);
});

test('added and removed blocks', () => {
	assert.equal(render('a\nc\n', 'a\nb\nc\nd\n'), ' a\n+b\n c\n+d');
	assert.equal(render('a\nb\nc\n', 'c\n'), '-a\n-b\n c');
});

test('empty sides: never synced, or everything removed', () => {
	assert.equal(render('', 'a\nb'), '+a\n+b');
	assert.equal(render('a\n', ''), '-a');
	assert.deepEqual(lineDiff('', ''), []);
});

test('applying the diff to a yields b', () => {
	const a = 'services:\n  web:\n    image: x\n    ports: [80]\n  db:\n    image: pg\n';
	const b = 'services:\n  db:\n    image: pg:17\n  web:\n    image: x\n    env_file: [w.env]\n';
	const d = lineDiff(a, b);
	const side = (skip: string) =>
		d
			.filter((l) => l.op !== skip)
			.map((l) => l.text)
			.join('\n') + '\n';
	assert.equal(side('+'), a);
	assert.equal(side('-'), b);
});
