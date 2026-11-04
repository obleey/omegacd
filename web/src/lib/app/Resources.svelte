<script lang="ts">
	import type { Container, LiveState } from '../api';
	import Badge from '../Badge.svelte';

	let { name, state, onlogs }: { name: string; state: LiveState | null; onlogs: (c: Container) => void } = $props();

	let services = $derived.by(() => {
		const cs = state?.containers ?? [];
		const names = [...new Set([...(state?.services ?? []), ...cs.map((c) => c.service)])].sort();
		return names.map((s) => ({ name: s, containers: cs.filter((c) => c.service === s) }));
	});
	let networks = $derived((state?.resources ?? []).filter((r) => r.kind === 'network'));
	let volumes = $derived((state?.resources ?? []).filter((r) => r.kind === 'volume'));
</script>

{#if !state}
	<p class="muted">Loading…</p>
{:else}
	<ul class="tree">
		<li>
			<div class="node app">
				<span class="kind">Application</span>
				<strong>{name}</strong>
				<Badge status={state.health} label="Health Status" />
			</div>
			<ul>
				{#each services as s (s.name)}
					<li>
						<div class="node">
							<span class="kind">Service</span>
							<span class="mono">{s.name}</span>
							{#if s.containers.length === 0}<Badge status="Missing" label="Health Status" />{/if}
						</div>
						{#if s.containers.length}
							<ul>
								{#each s.containers as c (c.id)}
									<li>
										<div class="node">
											<span class="kind">Container</span>
											<button class="link mono" onclick={() => onlogs(c)} title="Show logs for {c.name}">{c.name}</button>
											<Badge status={c.state} label="State" />
											<Badge status={c.health} label="Health Status" />
											<span class="muted small mono trunc" title={c.image}>{c.image}</span>
											<span class="muted small">{c.status}</span>
										</div>
									</li>
								{/each}
							</ul>
						{/if}
					</li>
				{/each}
				{#each [['Network', networks], ['Volume', volumes]] as const as [kind, list] (kind)}
					{#each list as r (r.name)}
						<li>
							<div class="node">
								<span class="kind">{kind}</span>
								<span class="mono">{r.name}</span>
							</div>
						</li>
					{/each}
				{/each}
			</ul>
		</li>
	</ul>
	{#if services.length === 0}<p class="muted small">No services yet. Sync the Application to create them.</p>{/if}
{/if}

<style>
	.tree, .tree ul { list-style: none; margin: 0; padding: 0; }
	.tree ul { margin-left: 1.1rem; border-left: 1px solid var(--surface1); padding-left: 0.9rem; }
	.tree li { margin: 0.35rem 0; }
	.node { display: flex; align-items: center; gap: 0.55rem; flex-wrap: wrap; padding: 0.35rem 0.6rem; border: 1px solid var(--surface0); border-radius: var(--radius); background: var(--mantle); width: fit-content; max-width: 100%; }
	.node.app { border-color: color-mix(in srgb, var(--accent) 50%, transparent); }
	.kind { font-size: 0.7rem; text-transform: uppercase; letter-spacing: 0.04em; color: var(--overlay1); min-width: 4.5rem; }
	.link { background: none; border: 0; padding: 0; color: var(--accent); }
	.link:hover:not(:disabled) { background: none; text-decoration: underline; }
	.trunc { max-width: 260px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
