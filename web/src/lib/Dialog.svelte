<script lang="ts">
	// Native <dialog> + showModal(): focus trap, Esc to close, inert background for free.
	import type { Snippet } from 'svelte';
	let {
		open = $bindable(false),
		title,
		wide = false,
		children,
		actions
	}: { open: boolean; title: string; wide?: boolean; children: Snippet; actions: Snippet } = $props();

	const uid = $props.id();
	let el: HTMLDialogElement;
	$effect(() => {
		if (open && !el.open) el.showModal();
		else if (!open && el.open) el.close();
	});
</script>

<dialog bind:this={el} class:wide onclose={() => (open = false)} aria-labelledby="{uid}-title">
	<h2 id="{uid}-title">{title}</h2>
	<div class="body">{@render children()}</div>
	<div class="row actions">{@render actions()}</div>
</dialog>

<style>
	dialog {
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		background: var(--elevated);
		color: var(--text);
		padding: 1.3rem 1.5rem;
		width: min(480px, 92vw);
		box-shadow: var(--shadow-lg);
	}
	dialog[open] { animation: pop 0.22s var(--ease); }
	dialog[open]::backdrop { animation: fadein 0.22s ease; }
	@keyframes pop { from { opacity: 0; transform: translateY(8px) scale(0.97); } }
	@keyframes fadein { from { opacity: 0; } }
	dialog.wide { width: min(860px, 94vw); }
	dialog::backdrop { background: rgb(17 17 27 / 0.55); backdrop-filter: blur(3px); }
	.body { margin: 0.8rem 0 1.2rem; }
	.actions { justify-content: flex-end; }
</style>
