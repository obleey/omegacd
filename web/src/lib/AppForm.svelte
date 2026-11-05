<script lang="ts">
	import { onMount } from 'svelte';
	import { slide } from 'svelte/transition';
	import { api, errMsg, get, type Application, type HostView, type RepoAccess, type RepoCredential } from './api.ts';
	import Icon from './Icon.svelte';
	import Dialog from './Dialog.svelte';
	import { ms } from './status.ts';

	let {
		initial,
		editing = false,
		submitLabel,
		onsave,
		oncancel
	}: {
		initial?: Application;
		editing?: boolean;
		submitLabel: string;
		onsave: (a: Application) => Promise<void>;
		oncancel?: () => void;
	} = $props();

	// Form state is seeded once from `initial`; later prop changes must not clobber edits.
	const init = (() => initial)();
	let name = $state(init?.name ?? '');
	let repoURL = $state(init?.repoURL ?? '');
	let path = $state(init?.path ?? '');
	let targetRevision = $state(init?.targetRevision || 'main');
	let composeFiles = $state((init?.composeFiles ?? ['compose.yaml']).join('\n'));
	let profiles = $state((init?.profiles ?? []).join('\n'));
	let autoSync = $state(init?.autoSync ?? false);
	let selfHeal = $state(init?.selfHeal ?? false);
	let prune = $state(init?.prune ?? false);
	let pruneVolumes = $state(init?.pruneVolumes ?? false);
	let askVolumes = $state(false);
	// Prune volumes is a sub-option of Prune: turning Prune off turns it off too
	$effect(() => {
		if (!prune) pruneVolumes = false;
	});
	let adopt = $state(init?.adopt ?? false);
	let kind = $state(init?.kind || 'compose');
	let host = $state(init?.host || 'local');
	let hosts = $state<string[]>([init?.host || 'local']);
	onMount(() => {
		get<HostView[]>('/settings/hosts')
			.then((hs) => {
				hosts = hs.map((h) => h.name);
				// no "local" when OmegaCD runs without a Docker engine (OMEGACD_LOCAL_HOST=false): preselect the first Host
				if (!init?.host && hosts.length && !hosts.includes(host)) host = hosts[0];
			})
			.catch(() => {}); // the select keeps the current Host
	});
	let error = $state('');
	let busy = $state(false);
	// An empty Path means the Repository root: ask once before creating, it is usually a mistake.
	let askRoot = $state(false);
	let pathEl = $state<HTMLInputElement>();

	// Repository access check: runs 600ms after typing stops and on blur; only the result for the current URL shows.
	let checkingURL = $state('');
	let result = $state<{ url: string; r: RepoAccess }>();
	const access = $derived(result && result.url === repoURL.trim() ? result.r : undefined);
	const checking = $derived(!access && checkingURL !== '' && checkingURL === repoURL.trim());
	const blocked = $derived(access?.access === 'denied' || access?.access === 'error');
	const betterBranch = $derived(
		access?.defaultBranch && access.defaultBranch !== 'main' && targetRevision.trim() === 'main' ? access.defaultBranch : ''
	);
	// The URL the user was asked to confirm creating with despite a failed check.
	let askAccess = $state('');

	async function check() {
		const url = repoURL.trim();
		if (!url || url === checkingURL || result?.url === url) return;
		checkingURL = url;
		let r: RepoAccess;
		try {
			r = await api<RepoAccess>('POST', '/repos/check', { repoURL: url });
		} catch (err) {
			r = { access: 'error', message: errMsg(err) };
		}
		if (checkingURL !== url) return; // a newer URL is being checked
		checkingURL = '';
		result = { url, r };
		if (r.access === 'denied') cred = { ...prefill(url), password: '', sshPrivateKey: '' };
	}
	$effect(() => {
		if (!repoURL.trim()) return;
		const t = setTimeout(check, 600);
		return () => clearTimeout(t);
	});

	const isSSH = (u: string) => u.startsWith('ssh://') || /^[^/:]+@[^/:]+:/.test(u);
	// Owner prefix: https://host/owner/ or git@host:owner/; the username defaults to the owner (the SSH user for ssh).
	function prefill(url: string) {
		const m = url.match(/^([a-z+]+:\/\/(?:([^@/]+)@)?[^/]+\/([^/]+)\/)/i);
		if (m) return { urlPrefix: m[1], username: isSSH(url) ? (m[2] ?? 'git') : m[3] };
		const s = url.match(/^(([^@/:]+)@[^/:]+:([^/]+)\/)/);
		if (s) return { urlPrefix: s[1], username: s[2] };
		return { urlPrefix: url, username: '' };
	}

	let cred = $state({ urlPrefix: '', username: '', password: '', sshPrivateKey: '' });
	let credBusy = $state(false);
	let credError = $state('');
	async function saveCred() {
		credBusy = true;
		credError = '';
		try {
			// a rejected credential with the same prefix is updated rather than duplicated (prefixes are unique)
			const existing = ((await get<RepoCredential[]>('/settings/repo-credentials')) ?? []).find((c) => c.urlPrefix === cred.urlPrefix.trim());
			const body = { ...cred, urlPrefix: cred.urlPrefix.trim() };
			if (existing) await api('PUT', `/settings/repo-credentials/${existing.id}`, body);
			else await api('POST', '/settings/repo-credentials', body);
			result = undefined;
			checkingURL = '';
			await check();
		} catch (err) {
			credError = errMsg(err);
		} finally {
			credBusy = false;
		}
	}
	// Enter in the credential box saves it instead of submitting the Application form.
	function enterSaves(e: KeyboardEvent) {
		if (e.key !== 'Enter') return;
		e.preventDefault();
		saveCred();
	}

	const list = (s: string) =>
		s
			.split(/[,\n]/)
			.map((x) => x.trim())
			.filter(Boolean);

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		error = '';
		if (blocked && askAccess !== repoURL.trim()) {
			askAccess = repoURL.trim();
			return;
		}
		if (!editing && !path.trim() && !askRoot) {
			askRoot = true;
			pathEl?.focus();
			return;
		}
		busy = true;
		try {
			await onsave({
				...init,
				kind,
				notInGit: init?.notInGit ?? false,
				name: name.trim(),
				repoURL: repoURL.trim(),
				path: path.trim(),
				targetRevision: targetRevision.trim() || 'main',
				// An App of Apps has no compose files; its children pick their own Host.
				composeFiles: kind === 'apps' ? [] : list(composeFiles).length ? list(composeFiles) : ['compose.yaml'],
				profiles: kind === 'apps' ? [] : list(profiles),
				host,
				autoSync,
				selfHeal,
				prune,
				pruneVolumes: kind === 'compose' && prune && pruneVolumes,
				adopt: kind === 'compose' && adopt
			});
		} catch (err) {
			error = errMsg(err);
		} finally {
			busy = false;
		}
	}
</script>

<form onsubmit={submit} class="form">
	{#if error}<p class="error" role="alert">{error}</p>{/if}

	{#if !editing}
		<fieldset class="kinds">
			<legend>Kind <span class="muted small">— what does Path hold?</span></legend>
			<div class="kgrid">
				<label class="kcard" class:on={kind === 'compose'}>
					<input type="radio" name="kind" value="compose" bind:group={kind} class="sr-only" />
					<span class="kicon"><Icon name="container" size={24} /></span>
					<span class="ktxt"><strong>Compose project</strong><span>Deploy a docker compose project to one Host.</span></span>
					<span class="kcheck" aria-hidden="true"><Icon name="check" size={13} /></span>
				</label>
				<label class="kcard" class:on={kind === 'apps'}>
					<input type="radio" name="kind" value="apps" bind:group={kind} class="sr-only" />
					<span class="kicon"><Icon name="apps" size={24} /></span>
					<span class="ktxt"><strong>App of Apps</strong><span>A path of Application definitions; Sync creates its Child Applications.</span></span>
					<span class="kcheck" aria-hidden="true"><Icon name="check" size={13} /></span>
				</label>
			</div>
			{#if kind === 'apps'}
				<div class="hint help" transition:slide={{ duration: ms(220) }}>
					<p>
						Each YAML file in Path defines one Child Application. Syncing creates and updates them; with Prune it also
						deletes the ones no longer declared. Each Child Application picks its own Host. For example:
					</p>
					<pre>name: web
repoURL: https://github.com/org/repo.git
path: apps/web
targetRevision: main
composeFiles: [compose.yaml]
profiles: []
host: local
autoSync: true
selfHeal: true
prune: false</pre>
				</div>
			{/if}
		</fieldset>
	{/if}

	<div class="field">
		<label for="f-name">Name</label>
		<input id="f-name" bind:value={name} required disabled={editing} pattern="[a-z0-9][a-z0-9_-]*" autocomplete="off" />
		<span class="hint">{kind === 'apps' ? '' : 'Also the Compose project name. '}Lowercase letters, digits, - and _.</span>
	</div>
	<div class="field">
		<label for="f-repo">Repository URL</label>
		<input id="f-repo" bind:value={repoURL} onblur={check} required placeholder="https://github.com/org/repo.git" autocomplete="off" aria-describedby="f-repo-status" />
		<span class="access" id="f-repo-status" aria-live="polite">
			{#if checking}
				<span class="st muted"><Icon name="notch" size={14} spin />Checking access…</span>
			{:else if access?.access === 'public'}
				<span class="st ok"><Icon name="check" size={14} />Public repository — no credentials needed</span>
			{:else if access?.access === 'credential'}
				<span class="st ok"><Icon name="key" size={14} />Private — using credential <code>{access.credentialPrefix}</code></span>
			{:else if access?.access === 'denied'}
				<span class="st bad"><Icon name="lock" size={14} />Access denied: {access.message}</span>
			{:else if access?.access === 'error'}
				<span class="st warn"><Icon name="alert" size={14} />Could not reach the repository: {access.message}</span>
			{/if}
		</span>
		{#if access?.access === 'denied'}
			{@const ssh = isSSH(repoURL.trim())}
			<div class="addcred" role="group" aria-labelledby="f-cred-title" transition:slide={{ duration: ms(200) }}>
				<p class="small"><strong id="f-cred-title">Add credential</strong> <span class="muted">— used for every Repository whose URL starts with the prefix.</span></p>
				<div class="grid2">
					<div class="field">
						<label for="f-cred-prefix">URL prefix</label>
						<input id="f-cred-prefix" bind:value={cred.urlPrefix} onkeydown={enterSaves} autocomplete="off" />
					</div>
					<div class="field">
						<label for="f-cred-user">Username</label>
						<input id="f-cred-user" bind:value={cred.username} onkeydown={enterSaves} autocomplete="off" />
					</div>
				</div>
				{#if ssh}
					<div class="field">
						<label for="f-cred-ssh">SSH private key</label>
						<textarea id="f-cred-ssh" bind:value={cred.sshPrivateKey} rows="3" placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"></textarea>
					</div>
				{:else}
					<div class="field">
						<label for="f-cred-pass">Password or token</label>
						<input id="f-cred-pass" type="password" bind:value={cred.password} onkeydown={enterSaves} autocomplete="new-password" aria-describedby="f-cred-hint" />
						<span class="hint" id="f-cred-hint">GitHub: fine-grained token with Contents: read-only.</span>
					</div>
				{/if}
				{#if credError}<p class="error" role="alert">{credError}</p>{/if}
				<button type="button" onclick={saveCred} disabled={credBusy || !cred.urlPrefix.trim() || !(ssh ? cred.sshPrivateKey : cred.password)}>
					{credBusy ? 'Saving…' : 'Save credential'}
				</button>
			</div>
		{/if}
	</div>
	{#if kind === 'compose'}
	<div class="field">
		<label for="f-host">Host</label>
		<select id="f-host" bind:value={host} disabled={editing}>
			{#each hosts as h (h)}<option value={h}>{h}</option>{/each}
		</select>
		<span class="hint">
			{editing ? 'To move the Application to another Host, delete and recreate it.' : 'The Docker engine to deploy to. Add Hosts in Settings.'}
		</span>
	</div>
	{/if}
	<div class="grid2">
		<div class="field path" class:ask={askRoot && !path.trim()}>
			<label for="f-path">Path <span class="req" aria-hidden="true">*</span></label>
			<input id="f-path" bind:this={pathEl} bind:value={path} placeholder={kind === 'apps' ? 'apps' : 'apps/myapp'} autocomplete="off" aria-describedby="f-path-hint" />
			{#if askRoot && !path.trim()}
				<span class="askroot" id="f-path-hint" role="alert" transition:slide={{ duration: ms(180) }}>
					<Icon name="alert" size={14} />Repository root — is that right? Submit again to use it, or enter a path.
				</span>
			{:else}
				<span class="hint" id="f-path-hint">
					Directory in the Repository that holds {kind === 'apps' ? 'Application definition YAML' : 'compose.yaml'}. Empty means the Repository root.
				</span>
			{/if}
		</div>
		<div class="field">
			<label for="f-rev">Target Revision</label>
			<input id="f-rev" bind:value={targetRevision} placeholder="main" autocomplete="off" />
			<span class="hint">Branch, tag or commit.</span>
			{#if betterBranch}
				<button type="button" class="usebranch" onclick={() => (targetRevision = betterBranch)}>
					<Icon name="branch" size={13} />Use {betterBranch} <span class="muted">— the Repository's default branch</span>
				</button>
			{/if}
		</div>
	</div>
	{#if kind === 'compose'}
	<div class="grid2">
		<div class="field">
			<label for="f-files">Compose files</label>
			<textarea id="f-files" bind:value={composeFiles} rows="3"></textarea>
			<span class="hint">One per line or comma-separated, relative to Path.</span>
		</div>
		<div class="field">
			<label for="f-prof">Profiles</label>
			<textarea id="f-prof" bind:value={profiles} rows="3"></textarea>
			<span class="hint">Optional. One per line or comma-separated.</span>
		</div>
	</div>
	{/if}

	<fieldset>
		<legend>Sync Policy</legend>
		<div class="check">
			<input id="f-auto" type="checkbox" bind:checked={autoSync} />
			<label for="f-auto">Auto-Sync <span class="hint">Sync automatically when the Repository moves to a new Revision.</span></label>
		</div>
		<div class="check">
			<input id="f-heal" type="checkbox" bind:checked={selfHeal} />
			<label for="f-heal">Self-Heal <span class="hint">Sync automatically when the Live State drifts without a new Revision.</span></label>
		</div>
		<div class="check">
			<input id="f-prune" type="checkbox" bind:checked={prune} />
			<label for="f-prune">Prune <span class="hint">Remove containers and networks no longer in the Desired State. Volumes are kept unless “also volumes” is on.</span></label>
		</div>
		{#if prune && kind === 'compose'}
			<div class="check nested" transition:slide={{ duration: ms(180) }}>
				<input
					id="f-prune-vol"
					type="checkbox"
					checked={pruneVolumes}
					onchange={(e) => {
						// turning it on needs a confirmation: keep it off until confirmed
						if (e.currentTarget.checked) {
							e.currentTarget.checked = false;
							askVolumes = true;
						} else pruneVolumes = false;
					}}
				/>
				<label for="f-prune-vol" class="danger">…also volumes <span class="hint">Deletes volumes no longer declared — data is lost.</span></label>
			</div>
		{/if}
		{#if !editing && kind === 'compose'}
			<div class="check">
				<input id="f-adopt" type="checkbox" bind:checked={adopt} />
				<label for="f-adopt">Adopt <span class="hint">Take over an existing, unmanaged Compose project with the same name.</span></label>
			</div>
		{/if}
	</fieldset>

	<Dialog bind:open={askVolumes} title="Prune volumes?">
		<p>Volumes this Application no longer declares will be <strong>deleted with all their data</strong> on the next Sync (unless a container still uses them).</p>
		{#snippet actions()}
			<button type="button" onclick={() => (askVolumes = false)}>Cancel</button>
			<button type="button" class="danger solid" onclick={() => ((pruneVolumes = true), (askVolumes = false))}>Prune volumes</button>
		{/snippet}
	</Dialog>

	{#if blocked && askAccess === repoURL.trim()}
		<p class="askroot" role="alert"><Icon name="alert" size={14} />OmegaCD can't read this repository yet — {editing ? 'save' : 'create'} anyway? Submit again to confirm.</p>
	{/if}
	<div class="row">
		<button class="primary" type="submit" disabled={busy}>{busy ? 'Saving…' : blocked && askAccess === repoURL.trim() ? `${submitLabel} anyway` : askRoot && !path.trim() && !editing ? `${submitLabel} with Repository root` : submitLabel}</button>
		{#if oncancel}<button type="button" onclick={oncancel}>Cancel</button>{/if}
	</div>
</form>

<style>
	.form { max-width: 760px; }
	.kinds { border: 0; padding: 0; margin: 0 0 1.4rem; }
	.kinds legend { font-size: 0.95rem; padding: 0; margin-bottom: 0.6rem; }
	.kgrid { display: grid; grid-template-columns: 1fr 1fr; gap: 0.8rem; }
	.kcard { position: relative; display: flex; gap: 0.85rem; align-items: flex-start; padding: 1rem 1rem 1rem 0.9rem; border: 1.5px solid var(--border); border-radius: var(--radius-lg); background: var(--bg); cursor: pointer; font-weight: 400; transition: border-color 0.15s, background-color 0.15s, box-shadow 0.15s, transform 0.15s var(--ease); }
	.kcard:hover { border-color: color-mix(in srgb, var(--accent) 45%, var(--border)); transform: translateY(-1px); }
	.kcard.on { border-color: var(--accent); background: color-mix(in srgb, var(--accent) 8%, var(--card)); box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 15%, transparent); }
	.kcard:has(:focus-visible) { outline: 2px solid var(--accent); outline-offset: 2px; }
	.kicon { flex: none; width: 44px; height: 44px; border-radius: 12px; display: grid; place-items: center; color: var(--subtext0); background: color-mix(in srgb, var(--surface0) 60%, transparent); transition: color 0.15s, background-color 0.15s; }
	.on .kicon { color: var(--accent); background: color-mix(in srgb, var(--accent) 16%, transparent); }
	.ktxt { display: grid; gap: 0.2rem; }
	.ktxt strong { font-size: 0.98rem; font-weight: 650; }
	.ktxt span { font-size: 0.82rem; color: var(--subtext0); }
	.kcheck { position: absolute; top: 0.6rem; right: 0.6rem; width: 20px; height: 20px; border-radius: 50%; display: grid; place-items: center; background: var(--accent); color: var(--crust); opacity: 0; transform: scale(0.5); transition: opacity 0.15s, transform 0.2s var(--ease); }
	.on .kcheck { opacity: 1; transform: none; }
	.req { display: inline; color: var(--accent); font-weight: 700; }
	.path input { border-color: color-mix(in srgb, var(--accent) 35%, var(--border)); }
	.ask input { border-color: var(--yellow); box-shadow: 0 0 0 3px color-mix(in srgb, var(--yellow) 20%, transparent); }
	.askroot { display: flex; gap: 0.35rem; align-items: center; font-size: 0.8rem; color: var(--yellow); font-weight: 550; }
	.grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 0 1rem; }
	fieldset { border: 1px solid var(--surface0); border-radius: 8px; padding: 0.8rem 1rem 0.2rem; margin: 0 0 1.2rem; }
	legend { padding: 0 0.4rem; font-weight: 600; }
	.help p { margin: 0 0 0.4rem; }
	.help pre { background: var(--crust); border: 1px solid var(--surface0); border-radius: var(--radius); padding: 0.5rem 0.8rem; margin: 0 0 0.8rem; overflow-x: auto; color: var(--text); }
	.access { min-height: 1.2rem; font-size: 0.82rem; }
	.st { display: flex; gap: 0.35rem; align-items: center; overflow-wrap: anywhere; }
	.st :global(svg) { flex: none; }
	.ok { color: var(--green); }
	.bad { color: var(--red); }
	.nested { margin-left: 1.6rem; }
	label.danger { color: var(--red); }
	.warn { color: var(--yellow); }
	.addcred { margin-top: 0.5rem; padding: 0.7rem 0.9rem 0.8rem; border: 1px dashed color-mix(in srgb, var(--accent) 45%, var(--border)); border-radius: var(--radius); background: color-mix(in srgb, var(--accent) 5%, var(--card)); }
	.addcred p { margin: 0 0 0.5rem; }
	.usebranch { justify-self: start; display: inline-flex; gap: 0.35rem; align-items: center; font-size: 0.8rem; padding: 0.2rem 0.55rem; }
	@media (max-width: 640px) { .grid2, .kgrid { grid-template-columns: 1fr; } }
</style>
