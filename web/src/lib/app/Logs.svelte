<script lang="ts">
	import { tick } from 'svelte';
	import { enc, type Container } from '../api.ts';
	import Empty from '../Empty.svelte';
	import { parseLine, type Line } from '../ansi.ts';

	let {
		app,
		containers,
		selected = $bindable('')
	}: { app: string; containers: Container[]; selected: string } = $props();

	const MAX = 5000;
	let lines = $state<Line[]>([]);
	let paused = $state(false);
	let held: string[] = [];
	let status = $state('');
	let box = $state<HTMLElement>();

	// per-viewer display preferences; storage can be unavailable (private mode), so defaults apply
	const pref = (k: string, d: boolean) => {
		try {
			const v = localStorage.getItem('omegacd.logs.' + k);
			return v === null ? d : v === '1';
		} catch {
			return d;
		}
	};
	let colors = $state(pref('colors', true));
	let wrap = $state(pref('wrap', true));
	let times = $state(pref('times', true));
	$effect(() => {
		try {
			localStorage.setItem('omegacd.logs.colors', colors ? '1' : '0');
			localStorage.setItem('omegacd.logs.wrap', wrap ? '1' : '0');
			localStorage.setItem('omegacd.logs.times', times ? '1' : '0');
		} catch {
			// not remembered; fine
		}
	});
	const clock = (ts: string) => ts.slice(11, 23);

	// A Sync recreates containers with new IDs: follow the selected service to its new container.
	let service = '';
	$effect(() => {
		const cur = containers.find((c) => c.id === selected);
		if (cur) {
			service = cur.service;
			return;
		}
		const next =
			containers.find((c) => c.service === service) ?? containers.find((c) => c.state === 'running') ?? containers[0];
		if (next) selected = next.id;
	});

	$effect(() => {
		if (!selected) return;
		lines = [];
		held = [];
		status = 'Connecting…';
		const es = new EventSource(`/api/apps/${enc(app)}/logs/${enc(selected)}?follow=1&tail=200`, { withCredentials: true });
		es.onopen = () => (status = 'Streaming');
		es.addEventListener('log', (e) => (paused ? held.push(e.data) : append([e.data])));
		es.addEventListener('end', () => {
			status = 'Stream ended';
			es.close();
		});
		// Server-sent "error" carries a message; a connection error has no data.
		es.addEventListener('error', (e) => {
			const msg = (e as MessageEvent).data;
			status = msg ? `Error: ${msg}` : 'Disconnected';
			es.close();
		});
		return () => es.close();
	});

	async function append(more: string[]) {
		lines.push(...more.map(parseLine));
		if (lines.length > MAX) lines.splice(0, lines.length - MAX);
		await tick();
		if (box && !paused) box.scrollTop = box.scrollHeight;
	}

	function togglePause() {
		paused = !paused;
		if (!paused) {
			append(held);
			held = [];
		}
	}
</script>

{#if containers.length === 0}
	<Empty icon="terminal" title="No containers">
		<p>Sync the Application first, then follow its container logs here.</p>
	</Empty>
{:else}
	<div class="row bar">
		<label for="logc" class="small">Container</label>
		<select id="logc" bind:value={selected}>
			{#each containers as c (c.id)}<option value={c.id}>{c.name} ({c.state})</option>{/each}
		</select>
		<span class="spacer"></span>
		<span class="muted small" role="status">{paused ? 'Paused' : status}</span>
		<label class="chk small"><input type="checkbox" bind:checked={colors} /> Colors</label>
		<label class="chk small"><input type="checkbox" bind:checked={wrap} /> Wrap</label>
		<label class="chk small"><input type="checkbox" bind:checked={times} /> Time</label>
		<button onclick={togglePause} aria-pressed={paused}>{paused ? 'Resume' : 'Pause'}</button>
		<button onclick={() => (lines = [])}>Clear</button>
	</div>
	<!-- svelte-ignore a11y_no_noninteractive_tabindex (focusable so keyboard users can scroll) -->
	<div class="logs mono" class:wrap class:plain={!colors} bind:this={box} tabindex="0" role="log" aria-label="Container logs">
		{#each lines as l, i (i)}
			<div class="ln lv-row-{l.level}">
				{#if times && l.ts}<span class="ts" title={l.ts}>{clock(l.ts)}</span>{/if}{#each l.segs as s, j (j)}<span class={s.cls} style={colors ? s.style : undefined}>{s.text}</span>{/each}
			</div>
		{/each}
	</div>
{/if}

<style>
	.bar { margin-bottom: 0.6rem; }
	select { width: auto; max-width: 320px; }
	.logs { margin: 0; height: 60vh; overflow: auto; background: var(--crust); color: var(--text); border: 1px solid var(--surface0); border-radius: var(--radius); padding: 0.5rem 0; font-size: 0.8rem; line-height: 1.45; }
	.ln { white-space: pre; padding: 0 0.8rem; border-left: 2px solid transparent; }
	.wrap .ln { white-space: pre-wrap; word-break: break-word; }
	.ln:hover { background: color-mix(in srgb, var(--surface0) 45%, transparent); }
	.ts { color: var(--overlay0); margin-right: 0.75rem; user-select: none; }
	.chk { display: inline-flex; align-items: center; gap: 0.3rem; color: var(--subtext0); }
	/* highlighting for plain lines (ANSI-colored lines use the program's own colors) */
	.logs:not(.plain) .lv-row-error { background: color-mix(in srgb, var(--red) 9%, transparent); border-left-color: var(--red); }
	.logs:not(.plain) .lv-row-warn { border-left-color: var(--yellow); }
	.logs:not(.plain) .lv-error { color: var(--red); font-weight: 700; }
	.logs:not(.plain) .lv-warn { color: var(--yellow); font-weight: 700; }
	.logs:not(.plain) .lv-info { color: var(--blue); }
	.logs:not(.plain) .lv-debug { color: var(--overlay1); }
	.logs:not(.plain) .lv-row-debug { color: var(--subtext0); }
	.logs:not(.plain) .key { color: var(--mauve); }
</style>
