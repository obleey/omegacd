<script lang="ts">
	// A link to the Git host's web page when there is one (opens in a new tab), plain text otherwise.
	import type { Snippet } from 'svelte';

	let { href, title, children }: { href?: string | null; title?: string; children: Snippet } = $props();
</script>

{#if href}<a class="gitlink" {href} {title} target="_blank" rel="noopener noreferrer">{@render children()}</a>{:else}<span {title}>{@render children()}</span>{/if}

<style>
	.gitlink { color: inherit; text-decoration: underline dotted color-mix(in srgb, currentColor 45%, transparent); text-underline-offset: 3px; }
	.gitlink:hover { color: var(--accent); text-decoration: underline; }
</style>
