import { test } from 'node:test';
import assert from 'node:assert/strict';
import { gitWeb } from './gitweb.ts';

test('GitHub https, with and without .git', () => {
	for (const u of ['https://github.com/obleey/omegacd.git', 'https://github.com/obleey/omegacd', 'https://github.com/obleey/omegacd/']) {
		const g = gitWeb(u)!;
		assert.equal(g.repo, 'https://github.com/obleey/omegacd');
		assert.equal(g.commit('abc'), 'https://github.com/obleey/omegacd/commit/abc');
		assert.equal(g.tree('abc', 'apps/web'), 'https://github.com/obleey/omegacd/tree/abc/apps/web');
		assert.equal(g.compare('a', 'b'), 'https://github.com/obleey/omegacd/compare/a...b');
	}
});

test('scp-like and ssh:// URLs map to the https web UI', () => {
	assert.equal(gitWeb('git@github.com:obleey/test-omegacd.git')!.repo, 'https://github.com/obleey/test-omegacd');
	assert.equal(gitWeb('ssh://git@gitlab.example.com:2222/team/app.git')!.repo, 'https://gitlab.example.com/team/app');
});

test('credentials in the URL never end up in a link', () => {
	assert.equal(gitWeb('https://user:secret@github.com/o/r.git')!.repo, 'https://github.com/o/r');
});

test('GitLab, Gitea/Forgejo and Bitbucket layouts', () => {
	assert.equal(gitWeb('https://gitlab.com/g/sub/r')!.commit('x'), 'https://gitlab.com/g/sub/r/-/commit/x');
	assert.equal(gitWeb('https://gitlab.com/g/r')!.tree('x', '.'), 'https://gitlab.com/g/r/-/tree/x');
	assert.equal(gitWeb('https://codeberg.org/o/r')!.tree('x', 'a'), 'https://codeberg.org/o/r/src/commit/x/a');
	assert.equal(gitWeb('http://git.localhost/o/r.git')!.repo, 'http://git.localhost/o/r', 'keeps http');
	assert.equal(gitWeb('https://git.example.com:8443/o/r')!.repo, 'https://git.example.com:8443/o/r', 'keeps the web port');
	assert.equal(gitWeb('https://bitbucket.org/o/r')!.commit('x'), 'https://bitbucket.org/o/r/commits/x');
});

test('local paths have no web page', () => {
	for (const u of ['/srv/git/repo', 'file:///srv/git/repo', './repo', '']) assert.equal(gitWeb(u), null);
});
