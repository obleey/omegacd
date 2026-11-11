import { test } from 'node:test';
import assert from 'node:assert/strict';
import { shortKey, sopsYaml } from './hostkeys.ts';

const r = 'age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqs3290gq';

test('one creation rule per Host; a Host whose Agent has not enrolled has no key yet', () => {
	const y = sopsYaml([{ name: 'local', recipient: r }, { name: 'prod', recipient: 'age1prod' }, { name: 'edge', recipient: '' }]);
	assert.equal(
		y,
		'creation_rules:\n' +
			'  # Host local; the path is up to you: match the files of the Applications deployed to it\n' +
			`  - path_regex: apps/local/.*\\.sops\\.\n    age: ${r}\n` +
			'  # Host prod; the path is up to you: match the files of the Applications deployed to it\n' +
			'  - path_regex: apps/prod/.*\\.sops\\.\n    age: age1prod\n' +
			'  # Host edge has no Host Key yet: its Agent makes one when it enrolls\n'
	);
});

test('shortKey keeps both ends', () => {
	assert.equal(shortKey(r), 'age1qyqszq…3290gq');
	assert.equal(shortKey('age1short'), 'age1short');
});
