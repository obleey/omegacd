import { test } from 'node:test';
import assert from 'node:assert/strict';
import { highlight, langFor, type Lang } from './syntax.ts';

// renders tokens as [cls:text] so expectations stay readable
const t = (line: string, lang: Lang) =>
	highlight(line, lang)
		.map((k) => (k.cls ? `[${k.cls.slice(3)}:${k.text}]` : k.text))
		.join('');

test('langFor', () => {
	assert.equal(langFor('compose.yaml'), 'yaml');
	assert.equal(langFor('apps/x/prometheus.yml'), 'yaml');
	assert.equal(langFor('.env'), 'env');
	assert.equal(langFor('grafana.env'), 'env');
	assert.equal(langFor('.env.local'), 'env');
	assert.equal(langFor('Caddyfile'), 'plain');
});

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
			highlight(l, 'yaml')
				.map((k) => k.text)
				.join(''),
			l
		);
	for (const l of ['A=1', 'export B="x y" ', '# c', 'not an env line', 'PW=••••'])
		assert.equal(
			highlight(l, 'env')
				.map((k) => k.text)
				.join(''),
			l
		);
});

test('yaml keys, values and comments', () => {
	assert.equal(t('services:', 'yaml'), '[key:services][punct::]');
	assert.equal(t('    image: nginx:1.27 # pinned', 'yaml'), '    [key:image][punct::] [val:nginx:1.27] [comment:# pinned]');
	assert.equal(t('  published: "8080"', 'yaml'), '  [key:published][punct::] [str:"8080"]');
	assert.equal(t('  target: 80', 'yaml'), '  [key:target][punct::] [num:80]');
	assert.equal(t('  target: 80  # http', 'yaml'), '  [key:target][punct::] [num:80]  [comment:# http]');
	assert.equal(t('  external: true', 'yaml'), '  [key:external][punct::] [bool:true]');
	assert.equal(t('  - mode: ingress', 'yaml'), '  [punct:- ][key:mode][punct::] [val:ingress]');
	assert.equal(t('  - ./a:/b:ro', 'yaml'), '  [punct:- ][val:./a:/b:ro]');
	assert.equal(t('  # note', 'yaml'), '  [comment:# note]');
	assert.equal(t('  url: http://h/a#frag', 'yaml'), '  [key:url][punct::] [val:http://h/a#frag]');
});

test('variables and masked secrets stand out', () => {
	assert.equal(t('  image: nginx:${TAG}', 'yaml'), '  [key:image][punct::] [val:nginx:][var:${TAG}]');
	assert.equal(t('  DB_PASSWORD: ••••', 'yaml'), '  [key:DB_PASSWORD][punct::] [masked:••••]');
	assert.equal(t('PW=••••', 'env'), '[key:PW][punct:=][masked:••••]');
});

test('env files', () => {
	assert.equal(t('A=1', 'env'), '[key:A][punct:=][num:1]');
	assert.equal(t('export URL="http://x"', 'env'), '[punct:export ][key:URL][punct:=][str:"http://x"]');
	assert.equal(t('B=x y ', 'env'), '[key:B][punct:=][val:x y] ');
	assert.equal(t('# comment', 'env'), '[comment:# comment]');
	assert.equal(t('garbage line', 'env'), 'garbage line');
});
