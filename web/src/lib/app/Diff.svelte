<script lang="ts">
	import { short, type DiffView } from '../api.ts';

	// apps: an App of Apps, whose diff entries are Child Applications rather than services
	let { diff, apps = false }: { diff: DiffView | null; apps?: boolean } = $props();

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
</script>

{#if !diff}
	<div class="loading" aria-busy="true" aria-label="Loading diff">
		<span class="skel" style="height: 24px; width: 40%"></span><span class="skel" style="height: 180px"></span>
	</div>
{:else}
	<h3>{apps ? 'Applications' : 'Services'}</h3>
	{#if diff.services?.length}
		<ul class="svc">
			{#each diff.services as s (s.service)}
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

	<h3>Git diff <span class="muted small mono">{short(diff.fromSHA)} → {short(diff.toSHA)}</span></h3>
	{#if lines.length}
		<pre class="diff" aria-label="Unified diff">{#each lines as l, i (i)}<span class={cls(l)}>{l || ' '}</span>{'\n'}{/each}</pre>
	{:else}
		<p class="muted small">No changes in Git between the deployed and target Revision.</p>
	{/if}
{/if}

<style>
	.loading { display: grid; gap: 0.7rem; }
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
