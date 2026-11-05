import { test } from 'node:test';
import assert from 'node:assert/strict';
import { highlight } from './syntax.ts';

// renders tokens as [cls:text] so expectations stay readable
const t = (line: string) =>
	highlight(line)
		.map((k) => (k.cls ? `[${k.cls.slice(3)}:${k.text}]` : k.text))
		.join('');

test('tokens always rebuild the exact line', () => {
	const yaml = [
		'services:',
		'  web:',
		'    image: nginx:${TAG:-1.27} # pinned',
		'    ports: ["8080:80"]',
		'      - mode: ingress',
		'    command: [sh, -c, "echo # not a comment"]',
		"  key: 'single # quoted'",
		'  - ./a:/b:ro',
		'#!',
		'   ',
		'x: |',
		'url: http://host:8080/a#frag',
		'- -1',
		'a: ••••',
		'  target: 80  # http  '
	];
	for (const l of yaml)
		assert.equal(
			highlight(l)
				.map((k) => k.text)
				.join(''),
			l
		);
});

test('yaml keys, values and comments', () => {
	assert.equal(t('services:'), '[key:services][punct::]');
	assert.equal(t('    image: nginx:1.27 # pinned'), '    [key:image][punct::] [val:nginx:1.27] [comment:# pinned]');
	assert.equal(t('  published: "8080"'), '  [key:published][punct::] [str:"8080"]');
	assert.equal(t('  target: 80'), '  [key:target][punct::] [num:80]');
	assert.equal(t('  target: 80  # http'), '  [key:target][punct::] [num:80]  [comment:# http]');
	assert.equal(t('  external: true'), '  [key:external][punct::] [bool:true]');
	assert.equal(t('  - mode: ingress'), '  [punct:- ][key:mode][punct::] [val:ingress]');
	assert.equal(t('  - ./a:/b:ro'), '  [punct:- ][val:./a:/b:ro]');
	assert.equal(t('  # note'), '  [comment:# note]');
	assert.equal(t('  url: http://h/a#frag'), '  [key:url][punct::] [val:http://h/a#frag]');
});

test('variables and masked secrets stand out', () => {
	assert.equal(t('  image: nginx:${TAG}'), '  [key:image][punct::] [val:nginx:][var:${TAG}]');
	assert.equal(t('  DB_PASSWORD: ••••'), '  [key:DB_PASSWORD][punct::] [masked:••••]');
});
