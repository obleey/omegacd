<script lang="ts" module>
	import type { IconName } from '../Icon.svelte';
	export type GNode = {
		id: string;
		/** layout parent (drawn as a solid edge); none = root column */
		parent?: string;
		/** extra parents, drawn as dashed edges */
		also?: string[];
		icon: IconName;
		kind: string;
		name: string;
		sub?: string;
		/** status values shown as icons (each with its own tooltip and screen reader text) */
		status?: [label: string, value: string][];
		tag?: string;
		dim?: boolean;
		href?: string;
		onclick?: () => void;
		action?: string; // accessible description of the click
	};
</script>

<script lang="ts">
	import { draw, scale } from 'svelte/transition';
	import Icon from '../Icon.svelte';
	import { meta, ms } from '../status.ts';

	let { nodes, label }: { nodes: GNode[]; label: string } = $props();

	const W = 250, H = 62, GAP = 14, COL = 72;

	// Tidy tree, left to right: leaves stack in rows, a parent is centred on its children.
	let layout = $derived.by(() => {
		const ids = new Set(nodes.map((n) => n.id));
		const kids = new Map<string, GNode[]>();
		const roots: GNode[] = [];
		for (const n of nodes) {
			if (n.parent && ids.has(n.parent)) kids.set(n.parent, [...(kids.get(n.parent) ?? []), n]);
			else roots.push(n);
		}
		const pos = new Map<string, { x: number; y: number; depth: number }>();
		let row = 0, maxDepth = 0;
		const place = (n: GNode, depth: number): number => {
			maxDepth = Math.max(maxDepth, depth);
			const ks = kids.get(n.id) ?? [];
			let y: number;
			if (!ks.length) y = row++ * (H + GAP);
			else {
				const ys = ks.map((k) => place(k, depth + 1));
				y = (ys[0] + ys[ys.length - 1]) / 2;
			}
			pos.set(n.id, { x: depth * (W + COL), y, depth });
			return y;
		};
		roots.forEach((r) => place(r, 0));
		const edges: { id: string; d: string; depth: number; dashed: boolean }[] = [];
		const edge = (from: string, to: string, dashed: boolean) => {
			const a = pos.get(from), b = pos.get(to);
			if (!a || !b) return;
			const x1 = a.x + W, y1 = a.y + H / 2, x2 = b.x, y2 = b.y + H / 2, dx = (x2 - x1) / 2;
			edges.push({ id: `${from}>${to}`, d: `M${x1} ${y1}C${x1 + dx} ${y1} ${x2 - dx} ${y2} ${x2} ${y2}`, depth: b.depth, dashed });
		};
		for (const n of nodes) {
			if (n.parent) edge(n.parent, n.id, false);
			for (const p of n.also ?? []) edge(p, n.id, true);
		}
		return { pos, edges, width: (maxDepth + 1) * (W + COL) - COL, height: Math.max(row, 1) * (H + GAP) - GAP };
	});
	// Columns appear one after another.
	const delay = (depth: number) => ms(depth * 220);
</script>

<div class="wrap" role="group" aria-label={label}>
	<div class="canvas" style:width="{layout.width}px" style:height="{layout.height}px">
		<svg width={layout.width} height={layout.height} aria-hidden="true">
			{#each layout.edges as e (e.id)}
				<path d={e.d} class:dashed={e.dashed} in:draw|global={{ duration: ms(550), delay: delay(e.depth - 1) + ms(120) }} />
			{/each}
		</svg>
		{#each nodes as n (n.id)}
			{@const p = layout.pos.get(n.id)}
			{#if p}
				{@const tone = meta(n.status?.at(-1)?.[1])[0]}
				<svelte:element
					this={n.href ? 'a' : n.onclick ? 'button' : 'div'}
					href={n.href}
					role={n.href ? 'link' : n.onclick ? 'button' : 'group'}
					type={n.onclick && !n.href ? 'button' : undefined}
					onclick={n.onclick}
					title={n.action}
					class="node {n.status ? tone : ''}"
					class:clickable={n.href || n.onclick}
					class:dim={n.dim}
					class:pulse={n.status?.some(([, v]) => meta(v)[2])}
					style:left="{p.x}px"
					style:top="{p.y}px"
					style:width="{W}px"
					style:height="{H}px"
					in:scale|global={{ start: 0.85, duration: ms(380), delay: delay(p.depth) }}
				>
					<span class="ico"><Icon name={n.icon} size={18} /></span>
					<span class="txt">
						<span class="kind">{n.kind}{#if n.tag}<span class="ntag">{n.tag}</span>{/if}</span>
						<span class="name" title={n.name}>{n.name}</span>
						{#if n.sub}<span class="sub" title={n.sub}>{n.sub}</span>{/if}
					</span>
					{#if n.status}
						<span class="st">
							{#each n.status as [l, v] (l)}
								{@const m = meta(v)}
								<span class="s {m[0]}" title="{l}: {v}"><Icon name={m[1]} size={15} spin={m[2]} /><span class="sr-only">{l}: {v}</span></span>
							{/each}
						</span>
					{/if}
				</svelte:element>
			{/if}
		{/each}
	</div>
</div>

<style>
	.wrap { overflow: auto; padding: 0.5rem 0.25rem 1rem; }
	.canvas { position: relative; margin: 0 auto 0 0; }
	svg { position: absolute; inset: 0; overflow: visible; }
	path { fill: none; stroke: color-mix(in srgb, var(--overlay0) 70%, transparent); stroke-width: 1.5; }
	path.dashed { stroke-dasharray: 4 4; opacity: 0.6; }
	.node {
		--c: var(--overlay1);
		position: absolute;
		display: flex;
		align-items: center;
		gap: 0.6rem;
		padding: 0 0.7rem 0 0.55rem;
		font: inherit;
		color: var(--text);
		text-align: left;
		background: var(--elevated);
		border: 1px solid var(--border);
		border-left: 3px solid var(--c);
		border-radius: 10px;
		box-shadow: var(--shadow);
		transition: left 0.4s var(--ease), top 0.4s var(--ease), transform 0.15s var(--ease), box-shadow 0.15s, border-color 0.15s;
		white-space: normal;
		justify-content: flex-start;
	}
	.clickable { cursor: pointer; }
	.clickable:hover { transform: translateY(-1px); box-shadow: var(--shadow-md); border-color: color-mix(in srgb, var(--accent) 50%, var(--border)); border-left-color: var(--c); text-decoration: none; background: var(--elevated); }
	.green { --c: var(--green); }
	.yellow { --c: var(--yellow); }
	.red { --c: var(--red); }
	.blue { --c: var(--blue); }
	.dim { opacity: 0.6; border-style: dashed; border-left-style: dashed; box-shadow: none; }
	.pulse { animation: pulse 1.6s var(--ease) infinite; }
	@keyframes pulse {
		0% { box-shadow: var(--shadow), 0 0 0 0 color-mix(in srgb, var(--c) 50%, transparent); }
		70%, 100% { box-shadow: var(--shadow), 0 0 0 8px color-mix(in srgb, var(--c) 0%, transparent); }
	}
	.ico { flex: none; width: 34px; height: 34px; border-radius: 9px; display: grid; place-items: center; color: var(--accent); background: color-mix(in srgb, var(--accent) 12%, transparent); }
	.txt { min-width: 0; flex: 1; display: grid; line-height: 1.25; }
	.kind { font-size: 0.64rem; text-transform: uppercase; letter-spacing: 0.07em; color: var(--subtext0); font-weight: 600; display: flex; gap: 0.35rem; align-items: center; }
	.ntag { text-transform: none; letter-spacing: 0; padding: 0 0.35rem; border-radius: 999px; color: var(--yellow); border: 1px solid color-mix(in srgb, var(--yellow) 45%, transparent); font-size: 0.64rem; }
	.name, .sub { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	.name { font-size: 0.88rem; font-weight: 600; letter-spacing: -0.005em; }
	.sub { font-size: 0.72rem; color: var(--subtext0); }
	.st { display: flex; gap: 0.3rem; flex: none; }
	.s { display: inline-flex; color: var(--c); }
</style>
