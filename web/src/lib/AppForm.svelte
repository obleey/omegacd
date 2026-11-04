<script lang="ts">
	import { onMount } from 'svelte';
	import { slide } from 'svelte/transition';
	import { errMsg, get, type Application, type HostView } from './api.ts';
	import Icon from './Icon.svelte';
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
	let adopt = $state(init?.adopt ?? false);
	let kind = $state(init?.kind || 'compose');
	let host = $state(init?.host || 'local');
	let hosts = $state<string[]>([init?.host || 'local']);
	onMount(() => {
		get<HostView[]>('/settings/hosts')
			.then((hs) => (hosts = hs.map((h) => h.name)))
			.catch(() => {}); // the select keeps the current Host
	});
	let error = $state('');
	let busy = $state(false);
	// An empty Path means the Repository root: ask once before creating, it is usually a mistake.
	let askRoot = $state(false);
	let pathEl = $state<HTMLInputElement>();

	const list = (s: string) =>
		s
			.split(/[,\n]/)
			.map((x) => x.trim())
			.filter(Boolean);

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		error = '';
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
		<input id="f-repo" bind:value={repoURL} required placeholder="https://github.com/org/repo.git" autocomplete="off" />
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
			<label for="f-prune">Prune <span class="hint">Remove containers and networks no longer in the Desired State. Volumes are never removed.</span></label>
		</div>
		{#if !editing && kind === 'compose'}
			<div class="check">
				<input id="f-adopt" type="checkbox" bind:checked={adopt} />
				<label for="f-adopt">Adopt <span class="hint">Take over an existing, unmanaged Compose project with the same name.</span></label>
			</div>
		{/if}
	</fieldset>

	<div class="row">
		<button class="primary" type="submit" disabled={busy}>{busy ? 'Saving…' : askRoot && !path.trim() && !editing ? `${submitLabel} with Repository root` : submitLabel}</button>
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
	@media (max-width: 640px) { .grid2, .kgrid { grid-template-columns: 1fr; } }
</style>
