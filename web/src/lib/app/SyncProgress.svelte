<script lang="ts">
	import { fade, fly, scale } from 'svelte/transition';
	import { ago, short, type ProgressEvent, type SyncOperation } from '../api.ts';
	import Icon from '../Icon.svelte';
	import { ms } from '../status.ts';

	let { op, progress }: { op: SyncOperation; progress: ProgressEvent[] } = $props();

	// Compose reports a verb per resource; finished verbs get a check, errors a cross, the rest spin.
	const state = (p: ProgressEvent) =>
		/error|fail/i.test(p.status)
			? 'err'
			: /^(done|pulled|created|started|running|healthy|recreated|removed|stopped|exited|skipped|exists|up.to.date|built)/i.test(p.status)
				? 'ok'
				: 'busy';
	let done = $derived(progress.filter((p) => state(p) === 'ok').length);
</script>

<section class="card prog" aria-live="polite" aria-label="Sync progress" transition:fly={{ y: -8, duration: ms(260) }}>
	<div class="head">
		<span class="spinner"><Icon name="sync" size={20} spin /></span>
		<div>
			<h2>Syncing to <span class="mono">{short(op.revision)}</span></h2>
			<p class="muted small">{op.trigger} · started {ago(op.startedAt)}</p>
		</div>
		<span class="spacer"></span>
		{#if progress.length}<span class="count num small">{done} / {progress.length} done</span>{/if}
	</div>
	<div class="bar" aria-hidden="true">
		<span class:indet={!progress.length} style:width={progress.length ? `${Math.max(6, (done / progress.length) * 100)}%` : undefined}></span>
	</div>
	{#if progress.length === 0}
		<p class="muted small">Preparing the Desired State…</p>
	{:else}
		<ul>
			{#each progress as p (p.resource)}
				{@const st = state(p)}
				<li in:fly={{ x: -10, duration: ms(240) }}>
					<span class="ic {st}">
						{#key st}
							<span class="k" in:scale={{ start: 0.4, duration: ms(260) }}>
								<Icon name={st === 'ok' ? 'check-circle' : st === 'err' ? 'x-circle' : 'notch'} size={15} spin={st === 'busy'} />
							</span>
						{/key}
					</span>
					<span class="mono res">{p.resource}</span>
					{#key p.status}<span class="st {st}" in:fade={{ duration: ms(200) }}>{p.status}</span>{/key}
					{#if p.text}<span class="muted small txt">{p.text}</span>{/if}
				</li>
			{/each}
		</ul>
	{/if}
</section>

<style>
	.prog { margin-bottom: 1rem; border-color: color-mix(in srgb, var(--blue) 40%, var(--border)); background: linear-gradient(180deg, color-mix(in srgb, var(--blue) 6%, var(--card)), var(--card)); }
	.head { display: flex; align-items: center; gap: 0.8rem; }
	.head p { margin: 0; }
	.spinner { color: var(--blue); width: 38px; height: 38px; display: grid; place-items: center; border-radius: 50%; background: color-mix(in srgb, var(--blue) 14%, transparent); }
	.count { color: var(--blue); font-weight: 600; }
	.bar { height: 4px; border-radius: 4px; background: color-mix(in srgb, var(--blue) 15%, transparent); overflow: hidden; margin: 0.8rem 0 0.7rem; position: relative; }
	.bar span { position: absolute; left: 0; top: 0; bottom: 0; border-radius: 4px; background: linear-gradient(90deg, var(--blue), var(--mauve)); transition: width 0.5s var(--ease); }
	.bar .indet { width: 35%; animation: indet 1.2s var(--ease) infinite; }
	@keyframes indet { from { left: -35%; } to { left: 100%; } }
	ul { list-style: none; padding: 0; margin: 0; display: grid; gap: 0.15rem; max-height: 260px; overflow: auto; }
	li { display: flex; align-items: center; gap: 0.6rem; padding: 0.25rem 0.3rem; border-radius: 6px; min-width: 0; }
	li:hover { background: color-mix(in srgb, var(--surface0) 40%, transparent); }
	.ic { display: grid; width: 16px; height: 16px; place-items: center; }
	.k { display: inline-flex; }
	.ok { color: var(--green); }
	.err { color: var(--red); }
	.busy { color: var(--blue); }
	.res { font-weight: 550; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	.st { font-size: 0.8rem; font-weight: 600; }
	.txt { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
