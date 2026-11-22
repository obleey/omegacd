import { test } from 'node:test';
import assert from 'node:assert/strict';
import { peopleFor, shortKey, sopsYaml } from './hostkeys.ts';

const r = 'age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqs3290gq';
const keys = [
	{ name: 'ana', recipient: 'age1ana', allHosts: true, hosts: [] },
	{ name: 'bo', recipient: 'age1bo', allHosts: false, hosts: ['prod'] }
];

test('one creation rule per Host, for its Host Key and the people covering it; a Host whose Agent has not enrolled has no key yet', () => {
	const y = sopsYaml([{ name: 'local', recipient: r }, { name: 'prod', recipient: 'age1prod' }, { name: 'edge', recipient: '' }], keys);
	assert.equal(
		y,
		'creation_rules:\n' +
			'  # Host local; the path is up to you: match the files of the Applications deployed to it\n' +
			'  # omegacd: Host local\n' +
			`  - path_regex: apps/local/.*\\.sops\\.\n    age: ${r},age1ana\n` +
			'  # Host prod; the path is up to you: match the files of the Applications deployed to it\n' +
			'  # omegacd: Host prod\n' +
			'  - path_regex: apps/prod/.*\\.sops\\.\n    age: age1prod,age1ana,age1bo\n' +
			'  # Host edge has no Host Key yet: its Agent makes one when it enrolls\n'
	);
});

test('peopleFor: keys for all Hosts and the ones naming it', () => {
	assert.deepEqual(peopleFor('prod', keys), ['age1ana', 'age1bo']);
	assert.deepEqual(peopleFor('edge', keys), ['age1ana']);
	assert.deepEqual(peopleFor('edge', []), []);
});

test('shortKey keeps both ends', () => {
	assert.equal(shortKey(r), 'age1qyqszq…3290gq');
	assert.equal(shortKey('age1short'), 'age1short');
});
