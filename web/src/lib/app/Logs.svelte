<script lang="ts">
	import { tick } from 'svelte';
	import { enc, type Container } from '../api';

	let {
		app,
		containers,
		selected = $bindable('')
	}: { app: string; containers: Container[]; selected: string } = $props();

	const MAX = 5000;
	let lines = $state<string[]>([]);
	let paused = $state(false);
	let held: string[] = [];
	let status = $state('');
	let box = $state<HTMLPreElement>();

	$effect(() => {
		if (!selected && containers.length) selected = containers[0].id;
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
		lines.push(...more);
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
	<p class="muted">No containers. Sync the Application first.</p>
{:else}
	<div class="row bar">
		<label for="logc" class="small">Container</label>
		<select id="logc" bind:value={selected}>
			{#each containers as c (c.id)}<option value={c.id}>{c.name} ({c.state})</option>{/each}
		</select>
		<span class="spacer"></span>
		<span class="muted small" role="status">{paused ? 'Paused' : status}</span>
		<button onclick={togglePause} aria-pressed={paused}>{paused ? 'Resume' : 'Pause'}</button>
		<button onclick={() => (lines = [])}>Clear</button>
	</div>
	<!-- svelte-ignore a11y_no_noninteractive_tabindex (focusable so keyboard users can scroll) -->
	<pre class="logs" bind:this={box} tabindex="0" aria-label="Container logs">{lines.join('\n')}</pre>
{/if}

<style>
	.bar { margin-bottom: 0.6rem; }
	select { width: auto; max-width: 320px; }
	.logs { margin: 0; height: 60vh; overflow: auto; background: var(--crust); border: 1px solid var(--surface0); border-radius: var(--radius); padding: 0.6rem 0.8rem; white-space: pre-wrap; word-break: break-all; line-height: 1.4; }
</style>
