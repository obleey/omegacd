<script lang="ts">
	import type { GitWeb } from '../gitweb.ts';
	import GitLink from '../GitLink.svelte';
	import { short, type DiffView } from '../api.ts';
	import { lineDiff } from '../linediff.ts';
	import { highlight } from '../syntax.ts';

	// apps: an App of Apps, whose diff entries are Child Applications rather than services
	// prune / pruneVolumes: the app's Sync Policy, to say what the next Sync does with undeclared networks and volumes
	let {
		diff,
		apps = false,
		web,
		prune = false,
		pruneVolumes = false
	}: { diff: DiffView | null; apps?: boolean; web?: GitWeb | null; prune?: boolean; pruneVolumes?: boolean } = $props();
	let services = $derived(diff?.services?.filter((s) => !s.kind) ?? []);
	let undeclared = $derived(diff?.services?.filter((s) => s.kind) ?? []);

	const cls = (l: string) =>
		l.startsWith('+++') || l.startsWith('---') || l.startsWith('diff ') || l.startsWith('index ')
			? 'meta'
			: l.startsWith('@@')
				? 'hunk'
				: l.startsWith('+')
					? 'add'
					: l.startsWith('-')
						? 'del'
						: '';
	let lines = $derived(diff?.gitDiff ? diff.gitDiff.replace(/\n$/, '').split('\n') : []);
	const changeLabel = { added: '+ added', removed: '− removed', changed: '~ changed' };

	// Live = the deployed Revision's files in the work dir (what the containers run from), Desired = the target
	// Revision; both rendered: compose files merged, variables filled in, secrets from a SOPS .env as ••••.
	let view = $state<'summary' | 'compose' | 'live' | 'desired'>('summary');
	let composeDiff = $derived(view === 'compose' && diff ? lineDiff(diff.live, diff.desired) : []);
	let composeChanged = $derived(!!diff && diff.live !== diff.desired);
	const opCls = { ' ': '', '+': 'add', '-': 'del' };
</script>

{#if !diff}
	<div class="loading" aria-busy="true" aria-label="Loading diff">
		<span class="skel" style="height: 24px; width: 40%"></span><span class="skel" style="height: 180px"></span>
	</div>
{:else}
	{#if !apps}
		<div class="seg views" role="group" aria-label="Diff view">
			<button aria-pressed={view === 'summary'} onclick={() => (view = 'summary')}>Summary</button>
			<button aria-pressed={view === 'compose'} onclick={() => (view = 'compose')}
				>Compose diff{#if composeChanged}<span class="dot" title="Desired differs from Live"></span>{/if}</button
			>
			<button aria-pressed={view === 'live'} onclick={() => (view = 'live')}>Live</button>
			<button aria-pressed={view === 'desired'} onclick={() => (view = 'desired')}>Desired</button>
		</div>
	{/if}
	{#if view === 'compose'}
		<p class="muted small">
			Live <span class="mono">{short(diff.fromSHA) || '—'}</span> (deployed) → Desired <span class="mono">{short(diff.toSHA)}</span>, rendered as
			OmegaCD runs them. Variables from an encrypted <span class="mono">.env</span> show as ••••.
		</p>
		{#if !composeChanged}
			<p class="muted small">No differences: the target Revision runs the same compose project.</p>
		{:else}
			<pre class="diff" aria-label="Compose diff">{#each composeDiff as l, i (i)}<span class={opCls[l.op]}><span class="op">{l.op}</span> {#each highlight(l.text, 'yaml') as k, j (j)}<span class={k.cls}>{k.text}</span>{/each}</span>{'\n'}{/each}</pre>
		{/if}
	{:else if view === 'live' || view === 'desired'}
		<p class="muted small">
			{#if view === 'live'}The deployed Revision <span class="mono">{short(diff.fromSHA) || '—'}</span>: the files on the Host the containers run from.
			{:else}The target Revision <span class="mono">{short(diff.toSHA)}</span>: what the next Sync deploys.{/if}
			Rendered: compose files merged, variables filled in, profiles applied; variables from an encrypted <span class="mono">.env</span> show as ••••.
		</p>
		{#if view === 'live' && !diff.live}
			<p class="muted small">Not deployed yet: there is no Live compose before the first Sync.</p>
		{:else}
			<pre class="diff plain" aria-label="{view} compose">{#each (view === 'live' ? diff.live : diff.desired).replace(/\n$/, '').split('\n') as l, i (i)}{#each highlight(l, 'yaml') as k, j (j)}<span class={k.cls}>{k.text}</span>{/each}{'\n'}{/each}</pre>
		{/if}
	{:else}
	<h3>{apps ? 'Applications' : 'Services'}</h3>
	{#if services.length}
		<ul class="svc">
			{#each services as s (s.service)}
				<li>
					<span class="chg {s.change}">{changeLabel[s.change] ?? s.change}</span>
					<span class="mono">{s.service}</span>
					{#if s.fields?.length}<span class="muted small">{s.fields.join(', ')}</span>{/if}
				</li>
			{/each}
		</ul>
	{:else}
		<p class="muted small">
			{apps ? 'Child Applications match the Application definitions in Git.' : 'Live State matches the last deployed Desired State.'}
		</p>
	{/if}

	{#if undeclared.length}
		<h3>Would be pruned</h3>
		<ul class="svc">
			{#each undeclared as s (s.kind + s.service)}
				<li><span class="chg removed">{s.kind}</span><span class="mono">{s.service}</span></li>
			{/each}
		</ul>
		<p class="muted small">
			No longer declared; never counted as drift, and kept while a container still uses them.
			{#if !prune}Prune is off: they are kept.
			{:else if pruneVolumes}Prune and Prune volumes are on: the next Sync deletes them, volumes with their data.
			{:else}Prune is on: the next Sync deletes the networks; volumes are kept (Prune volumes is off).{/if}
		</p>
	{/if}

	<h3>
		Git diff <span class="muted small mono"
			><GitLink href={diff.fromSHA && web?.commit(diff.fromSHA)}>{short(diff.fromSHA)}</GitLink> → <GitLink href={web?.commit(diff.toSHA)}>{short(diff.toSHA)}</GitLink>{#if web && diff.fromSHA}
				· <a href={web.compare(diff.fromSHA, diff.toSHA)} target="_blank" rel="noopener noreferrer">compare on the Git host</a>{/if}</span
		>
	</h3>
	{#if lines.length}
		<pre class="diff" aria-label="Unified diff">{#each lines as l, i (i)}<span class={cls(l)}>{l || ' '}</span>{'\n'}{/each}</pre>
	{:else}
		<p class="muted small">No changes in Git between the deployed and target Revision.</p>
	{/if}
{/if}
{/if}

<style>
	.loading { display: grid; gap: 0.7rem; }
	.views { margin-bottom: 0.8rem; }
	.dot { display: inline-block; width: 6px; height: 6px; border-radius: 50%; background: var(--yellow); margin-left: 0.4rem; vertical-align: middle; }
	pre.plain { padding: 0.6rem 0.8rem; }
	pre.plain span { display: inline; min-width: 0; padding: 0; }
	.diff .op { display: inline; min-width: 0; padding: 0; }
	.diff span span { display: inline; min-width: 0; padding: 0; }
	h3 { margin: 1rem 0 0.5rem; }
	h3:first-child { margin-top: 0; }
	.svc { list-style: none; padding: 0; margin: 0; display: grid; gap: 0.3rem; }
	.svc li { display: flex; gap: 0.7rem; align-items: baseline; }
	.chg { font-family: var(--mono); font-size: 0.8rem; min-width: 6.5rem; }
	.added { color: var(--green); }
	.removed { color: var(--red); }
	.changed { color: var(--yellow); }
	.diff { background: var(--crust); border: 1px solid var(--surface0); border-radius: var(--radius); padding: 0.6rem 0; overflow-x: auto; margin: 0; line-height: 1.45; }
	.diff span { display: inline-block; min-width: 100%; padding: 0 0.8rem; }
	.add { color: var(--green); background: color-mix(in srgb, var(--green) 10%, transparent); }
	.del { color: var(--red); background: color-mix(in srgb, var(--red) 10%, transparent); }
	.hunk { color: var(--blue); }
	.meta { color: var(--subtext0); font-weight: 600; }
</style>
