<script lang="ts">
	// The server's version: the Git tag its release was built from (or a dev description).
	import { get } from '#lib/api.ts';

	let version = $state('');
	get<{ version: string }>('/version')
		.then((r) => (version = r.version))
		.catch(() => {});
</script>

{#if version}
	<a
		class="version mono"
		href={version.startsWith('v') && !version.includes('-') ? `https://github.com/obleey/omegacd/releases/tag/${version}` : 'https://github.com/obleey/omegacd'}
		target="_blank"
		rel="noopener noreferrer"
		title="OmegaCD version — release notes on GitHub">{version}</a
	>
{/if}

<style>
	.version { font-size: 0.72rem; color: var(--overlay1); text-decoration: none; }
	.version:hover { color: var(--subtext1); text-decoration: underline; }
</style>
