<script lang="ts">
	import { enc, errMsg, get, type Container, type HealthStatus, type LiveState, type Resource, type ResourceDetail } from '../api.ts';
	import Badge from '../Badge.svelte';
	import Dialog from '../Dialog.svelte';
	import Icon, { type IconName } from '../Icon.svelte';
	import Empty from '../Empty.svelte';
	import Graph, { type GNode } from './Graph.svelte';
	import { langFor, lineToks, tokenize, type Tok } from '../highlight.ts';

	let {
		name,
		state: live,
		syncStatus,
		prunePending = [],
		onlogs
	}: { name: string; state: LiveState | null; syncStatus: string; prunePending?: string[]; onlogs: (c: Container) => void } = $props();

	// The graph needs width; small screens start with the list.
	let mode = $state<'graph' | 'list'>(matchMedia('(max-width: 640px)').matches ? 'list' : 'graph');
	const kindIcon: Record<Resource['kind'], IconName> = {
		volume: 'volume', network: 'network', env_file: 'file-text', config: 'file-cog', secret: 'lock', bind: 'folder'
	};
	const worst: HealthStatus[] = ['Degraded', 'Missing', 'Progressing', 'Healthy'];

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
	// undeclared networks/volumes are listed as "network <name>" / "volume <name>"
	const pendingRes = (r: Resource) => prunePending.includes(`${r.kind} ${r.name}`);
	let nodes = $derived.by((): GNode[] => {
		if (!live) return [];
		const out: GNode[] = [
			{ id: 'app', icon: 'app', kind: 'Application', name, status: [['Sync Status', syncStatus], ['Health Status', live.health]] }
		];
		for (const s of services) {
			const pending = prunePending.includes(s.name);
			const health = s.containers.length ? worst.find((h) => s.containers.some((c) => c.health === h)) ?? 'Healthy' : 'Missing';
			out.push({
				id: `s:${s.name}`, parent: 'app', icon: 'service', kind: 'Service', name: s.name,
				sub: pending ? 'Not declared, stopped' : `${s.containers.length} container${s.containers.length === 1 ? '' : 's'}`,
				status: pending ? undefined : [['Health Status', health]],
				dim: pending, tag: pending ? 'prune pending' : undefined
			});
			for (const c of s.containers)
				out.push({
					id: `c:${c.id}`, parent: `s:${s.name}`, icon: 'container', kind: 'Container', name: c.name, sub: c.status || c.image,
					status: [['State', c.state], ['Health Status', c.health]], onclick: () => onlogs(c), action: `Show logs for ${c.name}`
				});
		}
		for (const r of live.resources ?? []) {
			const svcs = (r.services ?? []).filter((x) => services.some((s) => s.name === x));
			const pending = pendingRes(r);
			out.push({
				id: `r:${r.kind}:${r.name}`, parent: svcs[0] ? `s:${svcs[0]}` : 'app', also: svcs.slice(1).map((x) => `s:${x}`),
				icon: kindIcon[r.kind], kind: kindLabel[r.kind], name: r.name, sub: pending ? 'Not declared' : r.target,
				tag: pending ? 'prune pending' : r.secret ? 'secret' : undefined, dim: pending,
				onclick: () => show(r), action: `Show details of ${r.name}`
			});
		}
		return out;
	});
	let byKind = $derived(Object.groupBy(live?.resources ?? [], (r) => r.kind));

	let open = $state(false);
	let sel = $state.raw<Resource | null>(null);
	let detail = $state.raw<ResourceDetail | null>(null);
	let err = $state('');
	let info = $derived(Object.entries(detail?.info ?? {}).filter(([, v]) => v).sort(([a], [b]) => a.localeCompare(b)));
	let lines = $derived(detail?.content ? detail.content.replace(/\n$/, '').split('\n') : []);
	let toks = $state.raw<Tok[][] | null>(null);
	$effect(() => {
		const d = detail;
		toks = null;
		const lang = d?.content ? (d.masked ? 'dotenv' : langFor(d.name, d.content)) : null;
		if (!d?.content || !lang) return;
		let stale = false;
		tokenize(d.content, lang)
			.then((t) => {
				if (!stale) toks = t;
			})
			.catch(() => {}); // stays plain text
		return () => (stale = true);
	});

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
	<div class="loading" aria-busy="true" aria-label="Loading resources">
		{#each [0, 1, 2] as i (i)}<span class="skel" style:width="{70 - i * 15}%" style:height="44px" style:margin-left="{i * 12}%"></span>{/each}
	</div>
{:else if services.length === 0 && !live.resources?.length}
	<Empty icon="container" title="No services yet">
		<p>Sync the Application to create its containers from the Desired State.</p>
	</Empty>
{:else}
	<div class="bar">
		<span class="muted small">{services.length} service{services.length === 1 ? '' : 's'} · {live.containers?.length ?? 0} container{live.containers?.length === 1 ? '' : 's'} · {live.resources?.length ?? 0} other resource{live.resources?.length === 1 ? '' : 's'}</span>
		<span class="spacer"></span>
		<div class="seg" role="group" aria-label="Resources view">
			<button aria-pressed={mode === 'graph'} onclick={() => (mode = 'graph')}><Icon name="tree" size={15} />Tree</button>
			<button aria-pressed={mode === 'list'} onclick={() => (mode = 'list')}><Icon name="list" size={15} />List</button>
		</div>
	</div>
	{#if mode === 'graph'}
		<Graph {nodes} label="Resource tree of {name}" />
		<p class="muted small hint">Click a container for its logs, or a volume, network or file for its details.</p>
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
							{#if prunePending.includes(s.name)}<span class="tag warn">prune pending</span>
							{:else if s.containers.length === 0}<Badge status="Missing" label="Health Status" />{/if}
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

	{#each groups as [kind, title] (kind)}
		{#if byKind[kind]?.length}
			<h3>{title}</h3>
			<div class="scroll-x"><table>
				<thead><tr><th>Name</th><th>Used by</th><th>Target</th></tr></thead>
				<tbody>
					{#each byKind[kind] ?? [] as r (r.name)}
						<tr>
							<td>
								<button class="link mono" onclick={() => show(r)} title="Show details of {r.name}">{r.name}</button>
								{#if r.secret}<span class="secret"><Icon name="lock" size={11} /> secret</span>{/if}
								{#if pendingRes(r)}<span class="tag warn">prune pending</span>{/if}
							</td>
							<td class="mono small">{r.services?.join(', ') || '—'}</td>
							<td class="mono small">{r.target || '—'}</td>
						</tr>
					{/each}
				</tbody>
			</table></div>
		{/if}
	{/each}
	{/if}
{/if}

<Dialog bind:open wide title="{sel ? kindLabel[sel.kind] : ''} {sel?.name ?? ''}">
	{#if err}
		<p class="error">{err}</p>
	{:else if !detail}
			<div class="loading"><span class="skel" style="height: 90px"></span><span class="skel" style="height: 160px"></span></div>
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
			<p class="banner small"><Icon name="lock" size={13} /> Content is never shown for secrets.</p>
		{:else if detail.info?.type === 'directory'}
			<p class="muted small">Directory: its contents are not shown.</p>
		{/if}
		{#if lines.length}
			<pre class="code" aria-label="Content of {detail.name}">{#each lines as l, i (i)}<span class="ln" aria-hidden="true">{i + 1}</span>{#each lineToks(toks, i, l) as t, k (k)}<span class="tk" style={t.style}>{t.text}</span>{/each}{'\n'}{/each}</pre>
		{:else if detail.info?.type === 'file' && !detail.secret}
			<p class="muted small">Empty file.</p>
		{/if}
	{/if}
	{#snippet actions()}
		<button onclick={() => (open = false)}>Close</button>
	{/snippet}
</Dialog>

<style>
	.bar { display: flex; align-items: center; gap: 0.6rem; flex-wrap: wrap; margin-bottom: 0.6rem; }
	.hint { margin: 0.2rem 0 0; }
	.loading { display: grid; gap: 0.8rem; }
	h3 { margin: 1.2rem 0 0.5rem; }
	h3:first-child { margin-top: 0; }
	.tree, .tree ul { list-style: none; margin: 0; padding: 0; }
	.tree ul { margin-left: 1.1rem; border-left: 1px solid var(--surface1); padding-left: 0.9rem; }
	.tree li { margin: 0.35rem 0; }
	.node { display: flex; background: var(--elevated); align-items: center; gap: 0.55rem; flex-wrap: wrap; padding: 0.35rem 0.6rem; border: 1px solid var(--surface0); border-radius: var(--radius); width: fit-content; max-width: 100%; }
	.node.app { border-color: color-mix(in srgb, var(--accent) 50%, transparent); }
	.kind { font-size: 0.7rem; text-transform: uppercase; letter-spacing: 0.04em; color: var(--overlay1); min-width: 4.5rem; }
	.link { background: none; border: 0; box-shadow: none; padding: 0; color: var(--accent); white-space: normal; text-align: left; }
	.link:hover:not(:disabled) { background: none; text-decoration: underline; }
	.trunc { max-width: 260px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	.secret { margin-left: 0.5rem; font-size: 0.75rem; color: var(--peach); border: 1px solid color-mix(in srgb, var(--peach) 40%, transparent); border-radius: 999px; padding: 0 0.45rem; white-space: nowrap; }
	.info { margin-bottom: 0.8rem; }
	.info th { width: 1%; white-space: nowrap; text-transform: none; }
	.info td { word-break: break-all; }
	.code { background: var(--crust); border: 1px solid var(--surface0); border-radius: var(--radius); padding: 0.6rem 0.8rem; overflow: auto; max-height: 55vh; margin: 0.6rem 0 0; line-height: 1.45; }
	.ln { display: inline-block; min-width: 2.5rem; margin-right: 0.8rem; text-align: right; color: var(--overlay0); user-select: none; }
</style>
