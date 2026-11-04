<script lang="ts">
	import { gitWeb } from './gitweb.ts';
	import GitLink from './GitLink.svelte';
	import { ago, enc, short, type AppView } from './api.ts';
	import Badge from './Badge.svelte';
	import Icon from './Icon.svelte';
	import { appTone, isSyncing, shownSync } from './status.ts';
	import { refreshApp, syncApp } from './actions.ts';
	import RefreshNote from './RefreshNote.svelte';
	import PolicyChips from './PolicyChips.svelte';

	let { a, kids }: { a: AppView; kids?: number } = $props();
	let busy = $state('');
	let op = $derived(a.lastOperation);
	let running = $derived(isSyncing(a));

	async function run(what: 'sync' | 'refresh') {
		busy = what;
		await (what === 'sync' ? syncApp(a.name) : refreshApp(a.name));
		busy = '';
	}
	let web = $derived(gitWeb(a.repoURL));
</script>

<article class="tile {appTone(a)}" class:running>
	<header>
		<Icon name={a.kind === 'apps' ? 'apps' : 'app'} size={18} />
		<h3><a class="stretch" href="/apps/{enc(a.name)}">{a.name}</a></h3>
		{#if a.kind === 'apps'}<span class="tag">App of Apps</span>{/if}
		{#if a.notInGit}<span class="tag warn">not in Git</span>{/if}
	</header>
	<div class="status">
		<Badge status={shownSync(a)} label="Sync Status" />
		<Badge status={a.health} label="Health Status" />
		<PolicyChips {a} />
	</div>
	<dl>
		{#if a.kind === 'apps'}
			<dt><Icon name="apps" size={13} />Children</dt>
			<dd>{kids ?? 0} Child Application{kids === 1 ? '' : 's'}</dd>
		{:else}
			<dt><Icon name="server" size={13} />Host</dt>
			<dd class="mono">{a.host || 'local'}</dd>
		{/if}
		<dt><Icon name="folder" size={13} />Repository</dt>
		<dd class="mono trunc" title="{a.repoURL} {a.path}">
			<GitLink href={web?.repo}>{a.repoURL.replace(/^https?:\/\//, '').replace(/\.git$/, '')}</GitLink>{#if a.path && a.path !== '.'} · <GitLink href={web && a.targetSHA ? web.tree(a.targetSHA, a.path) : null}>{a.path}</GitLink>{/if}
		</dd>
		<dt><Icon name="branch" size={13} />Revision</dt>
		<dd class="mono">{a.targetRevision} <span class="muted"><GitLink href={a.targetSHA && web?.commit(a.targetSHA)} title="Open commit {a.targetSHA}">{short(a.targetSHA)}</GitLink></span></dd>
		<dt><Icon name="clock" size={13} />Last Sync</dt>
		<dd>
			{#if op}
				<Badge status={op.phase} size="sm" label="Last Sync Operation" />
				<span class="muted small">{ago(op.finishedAt || op.startedAt)}</span>
			{:else}
				<span class="muted">Never</span>
			{/if}
		</dd>
	</dl>
	<footer>
		<button class="sm" onclick={() => run('sync')} disabled={!!busy || running} aria-label="Sync {a.name}">
			<Icon name="sync" size={14} spin={running} />{running ? 'Syncing' : 'Sync'}
		</button>
		<button class="sm ghost" onclick={() => run('refresh')} disabled={!!busy} aria-label="Refresh {a.name}">
			<Icon name="refresh" size={14} spin={busy === 'refresh'} />Refresh
		</button>
		<RefreshNote app={a.name} />
	</footer>
</article>

<style>
	.tile {
		--c: var(--green);
		position: relative;
		height: 100%;
		display: flex;
		flex-direction: column;
		gap: 0.7rem;
		padding: 0.95rem 1rem 0.8rem 1.25rem;
		background: var(--card);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		box-shadow: var(--shadow);
		transition: transform 0.2s var(--ease), box-shadow 0.2s, border-color 0.2s;
		overflow: hidden;
	}
	.tile::before { content: ''; position: absolute; left: 0; top: 0; bottom: 0; width: 4px; background: var(--c); transition: background-color 0.4s; }
	.tile:hover { transform: translateY(-2px); box-shadow: var(--shadow-md); border-color: color-mix(in srgb, var(--c) 40%, var(--border)); }
	.tile:focus-within { border-color: color-mix(in srgb, var(--accent) 60%, var(--border)); }
	.green { --c: var(--green); }
	.yellow { --c: var(--yellow); }
	.red { --c: var(--red); }
	.blue { --c: var(--blue); }
	.overlay { --c: var(--overlay1); }
	.running { overflow: visible; animation: ring 1.8s var(--ease) infinite; }
	@keyframes ring {
		0% { box-shadow: var(--shadow), 0 0 0 0 color-mix(in srgb, var(--blue) 45%, transparent); }
		70%, 100% { box-shadow: var(--shadow), 0 0 0 9px color-mix(in srgb, var(--blue) 0%, transparent); }
	}
	.running::before { border-radius: var(--radius-lg) 0 0 var(--radius-lg); }
	header { display: flex; align-items: center; gap: 0.5rem; color: var(--subtext0); min-width: 0; }
	h3 { font-size: 1.02rem; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	h3 a { color: var(--text); }
	/* the name link covers the tile; buttons sit above it */
	.stretch::after { content: ''; position: absolute; inset: 0; z-index: 1; }
	.stretch:focus-visible { outline: none; }
	.stretch:focus-visible::after { outline: 2px solid var(--accent); outline-offset: -2px; border-radius: var(--radius-lg); }
	.status { display: flex; gap: 0.4rem; flex-wrap: wrap; align-items: center; }
	dl { display: grid; grid-template-columns: auto minmax(0, 1fr); gap: 0.32rem 0.8rem; margin: 0; padding-top: 0.7rem; border-top: 1px dashed var(--border); font-size: 0.85rem; }
	dt { display: flex; align-items: center; gap: 0.35rem; color: var(--subtext0); font-size: 0.78rem; }
	dd { margin: 0; display: flex; align-items: center; gap: 0.4rem; min-width: 0; }
	.trunc { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	footer { display: flex; gap: 0.4rem; margin-top: auto; padding-top: 0.2rem; position: relative; z-index: 2; }
</style>
