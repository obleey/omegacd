// Web links for a Repository: the repo page, a commit, a folder at a Revision, a compare view.
// Returns null for repos without a web UI we can derive (local paths, file://).

export type GitWeb = {
	repo: string;
	commit: (sha: string) => string;
	tree: (rev: string, path: string) => string;
	compare: (from: string, to: string) => string;
};

type Flavor = 'github' | 'gitlab' | 'bitbucket' | 'gitea';

function flavor(host: string): Flavor {
	const h = host.toLowerCase();
	if (h.includes('gitlab')) return 'gitlab';
	if (h.includes('bitbucket')) return 'bitbucket';
	if (h.includes('gitea') || h.includes('forgejo') || h.includes('codeberg') || h.startsWith('git.')) return 'gitea';
	return 'github'; // github.com and unknown hosts: the most common URL layout
}

/** Parses https://host/owner/repo(.git), git@host:owner/repo(.git) and ssh://[user@]host[:port]/owner/repo(.git). */
export function gitWeb(url: string): GitWeb | null {
	const u = url.trim();
	let host: string, path: string;
	let scheme = 'https'; // ssh and scp-like URLs: the web UI is (almost always) https on the same host
	let m = /^(https?):\/\/(?:[^@/]+@)?([^/]+)\/(.+)$/i.exec(u);
	if (m) [, scheme, host, path] = m;
	else if ((m = /^ssh:\/\/(?:[^@/]+@)?([^/:]+)(?::\d+)?\/(.+)$/i.exec(u))) [, host, path] = m;
	else if ((m = /^[^@/\s]+@([^:/\s]+):(.+)$/.exec(u))) [, host, path] = m;
	else return null;
	path = path.replace(/\/+$/, '').replace(/\.git$/i, '');
	if (!path.includes('/')) return null;
	// http(s) URLs keep their scheme and port; an ssh port is not the web port
	if (scheme === 'https' && !/^https?:/i.test(u)) host = host.replace(/:\d+$/, '');
	const repo = `${scheme.toLowerCase()}://${host}/${path}`;
	const f = flavor(host.replace(/:\d+$/, ''));
	const clean = (p: string) => p.replace(/^\.?\/*/, '').replace(/\/+$/, '');
	return {
		repo,
		commit: (sha) =>
			f === 'gitlab' ? `${repo}/-/commit/${sha}` : f === 'bitbucket' ? `${repo}/commits/${sha}` : `${repo}/commit/${sha}`,
		tree: (rev, p) => {
			const sub = clean(p) ? '/' + clean(p) : '';
			if (f === 'gitlab') return `${repo}/-/tree/${rev}${sub}`;
			if (f === 'bitbucket') return `${repo}/src/${rev}${sub}`;
			if (f === 'gitea') return `${repo}/src/commit/${rev}${sub}`;
			return `${repo}/tree/${rev}${sub}`;
		},
		compare: (from, to) =>
			f === 'gitlab'
				? `${repo}/-/compare/${from}...${to}`
				: f === 'bitbucket'
					? `${repo}/branches/compare/${to}%0D${from}`
					: `${repo}/compare/${from}...${to}`
	};
}
