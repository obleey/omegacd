<script lang="ts">
	import { enc, errMsg, get, type Container, type LiveState, type Resource, type ResourceDetail } from '../api';
	import Badge from '../Badge.svelte';
	import Dialog from '../Dialog.svelte';

	let { name, state: live, onlogs }: { name: string; state: LiveState | null; onlogs: (c: Container) => void } = $props();

	const groups: [Resource['kind'], string][] = [
		['volume', 'Volumes'],
		['network', 'Networks'],
		['env_file', 'Env files'],
		['config', 'Configs'],
		['secret', 'Secrets'],
		['bind', 'Bind mounts']
	];
	const kindLabel = Object.fromEntries(groups.map(([k, t]) => [k, t.replace(/s$/, '')]));

	let services = $derived.by(() => {
		const cs = live?.containers ?? [];
		const names = [...new Set([...(live?.services ?? []), ...cs.map((c) => c.service)])].sort();
		return names.map((s) => ({ name: s, containers: cs.filter((c) => c.service === s) }));
	});
	let byKind = $derived(Object.groupBy(live?.resources ?? [], (r) => r.kind));

	let open = $state(false);
	let sel = $state.raw<Resource | null>(null);
	let detail = $state.raw<ResourceDetail | null>(null);
	let err = $state('');
	let info = $derived(Object.entries(detail?.info ?? {}).filter(([, v]) => v).sort(([a], [b]) => a.localeCompare(b)));
	let lines = $derived(detail?.content ? detail.content.replace(/\n$/, '').split('\n') : []);

	async function show(r: Resource) {
		sel = r;
		detail = null;
		err = '';
		open = true;
		try {
			const d = await get<ResourceDetail>(`/apps/${enc(name)}/resource?kind=${enc(r.kind)}&name=${enc(r.name)}`);
			if (sel === r) detail = d;
		} catch (e) {
			if (sel === r) err = errMsg(e);
		}
	}
</script>

{#if !live}
	<p class="muted">Loading…</p>
{:else}
	<h3>Containers</h3>
	<ul class="tree">
		<li>
			<div class="node app">
				<span class="kind">Application</span>
				<strong>{name}</strong>
				<Badge status={live.health} label="Health Status" />
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
			</ul>
		</li>
	</ul>
	{#if services.length === 0}<p class="muted small">No services yet. Sync the Application to create them.</p>{/if}

	{#each groups as [kind, title] (kind)}
		{#if byKind[kind]?.length}
			<h3>{title}</h3>
			<table>
				<thead><tr><th>Name</th><th>Used by</th><th>Target</th></tr></thead>
				<tbody>
					{#each byKind[kind] ?? [] as r (r.name)}
						<tr>
							<td>
								<button class="link mono" onclick={() => show(r)} title="Show details of {r.name}">{r.name}</button>
								{#if r.secret}<span class="secret"><span aria-hidden="true">🔒</span> secret</span>{/if}
							</td>
							<td class="mono small">{r.services?.join(', ') || '—'}</td>
							<td class="mono small">{r.target || '—'}</td>
						</tr>
					{/each}
				</tbody>
			</table>
		{/if}
	{/each}
{/if}

<Dialog bind:open wide title="{sel ? kindLabel[sel.kind] : ''} {sel?.name ?? ''}">
	{#if err}
		<p class="error">{err}</p>
	{:else if !detail}
		<p class="muted">Loading…</p>
	{:else}
		{#if info.length}
			<table class="info">
				<tbody>
					{#each info as [k, v] (k)}<tr><th scope="row">{k}</th><td class="mono small">{v}</td></tr>{/each}
				</tbody>
			</table>
		{/if}
		{#if detail.masked}
			<p class="banner small">Decrypted from SOPS: only the keys are shown, every value is hidden.</p>
		{:else if detail.secret}
			<p class="banner small"><span aria-hidden="true">🔒</span> Content is never shown for secrets.</p>
		{:else if detail.info?.type === 'directory'}
			<p class="muted small">Directory: its contents are not shown.</p>
		{/if}
		{#if lines.length}
			<pre class="code" aria-label="Content of {detail.name}">{#each lines as l, i (i)}<span class="ln" aria-hidden="true">{i + 1}</span>{l}{'\n'}{/each}</pre>
		{:else if detail.info?.type === 'file' && !detail.secret}
			<p class="muted small">Empty file.</p>
		{/if}
	{/if}
	{#snippet actions()}
		<button onclick={() => (open = false)}>Close</button>
	{/snippet}
</Dialog>

<style>
	h3 { margin: 1.2rem 0 0.5rem; }
	h3:first-child { margin-top: 0; }
	.tree, .tree ul { list-style: none; margin: 0; padding: 0; }
	.tree ul { margin-left: 1.1rem; border-left: 1px solid var(--surface1); padding-left: 0.9rem; }
	.tree li { margin: 0.35rem 0; }
	.node { display: flex; align-items: center; gap: 0.55rem; flex-wrap: wrap; padding: 0.35rem 0.6rem; border: 1px solid var(--surface0); border-radius: var(--radius); background: var(--mantle); width: fit-content; max-width: 100%; }
	.node.app { border-color: color-mix(in srgb, var(--accent) 50%, transparent); }
	.kind { font-size: 0.7rem; text-transform: uppercase; letter-spacing: 0.04em; color: var(--overlay1); min-width: 4.5rem; }
	.link { background: none; border: 0; padding: 0; color: var(--accent); white-space: normal; text-align: left; }
	.link:hover:not(:disabled) { background: none; text-decoration: underline; }
	.trunc { max-width: 260px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	.secret { margin-left: 0.5rem; font-size: 0.75rem; color: var(--peach); border: 1px solid color-mix(in srgb, var(--peach) 40%, transparent); border-radius: 999px; padding: 0 0.45rem; white-space: nowrap; }
	.info { margin-bottom: 0.8rem; }
	.info th { width: 1%; white-space: nowrap; text-transform: none; }
	.info td { word-break: break-all; }
	.code { background: var(--crust); border: 1px solid var(--surface0); border-radius: var(--radius); padding: 0.6rem 0.8rem; overflow: auto; max-height: 55vh; margin: 0.6rem 0 0; line-height: 1.45; }
	.ln { display: inline-block; min-width: 2.5rem; margin-right: 0.8rem; text-align: right; color: var(--overlay0); user-select: none; }
</style>
