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
		border: 1px solid var(--surface1);
		border-radius: 10px;
		background: var(--mantle);
		color: var(--text);
		padding: 1.2rem 1.4rem;
		width: min(460px, 92vw);
	}
	dialog.wide { width: min(860px, 94vw); }
	dialog::backdrop { background: rgb(0 0 0 / 0.5); }
	.body { margin: 0.8rem 0 1.2rem; }
	.actions { justify-content: flex-end; }
</style>
