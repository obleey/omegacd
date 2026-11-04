<script lang="ts">
	// dockercd mark: a container cube inside a Sync loop.
	let { size = 28 }: { size?: number } = $props();
	const uid = $props.id();
</script>

<svg width={size} height={size} viewBox="0 0 32 32" aria-hidden="true" class="logo">
	<defs>
		<linearGradient id="{uid}-g" x1="0" y1="0" x2="1" y2="1">
			<stop offset="0" stop-color="var(--mauve)" />
			<stop offset="1" stop-color="var(--blue)" />
		</linearGradient>
	</defs>
	<rect width="32" height="32" rx="8" fill="url(#{uid}-g)" />
	<g fill="none" stroke="var(--crust)" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
		<path d="M16 10.5l5 2.8v5.4l-5 2.8-5-2.8v-5.4Z M11 13.3l5 2.8 5-2.8 M16 16.1v5.4" />
		<path d="M24.5 13a9 9 0 0 0-15.6-3.6 M7.5 19a9 9 0 0 0 15.6 3.6" opacity="0.85" />
		<path d="M8.9 6.2v3.2h3.2 M23.1 25.8v-3.2h-3.2" opacity="0.85" />
	</g>
</svg>

<style>
	.logo { flex: none; display: block; filter: drop-shadow(0 4px 10px color-mix(in srgb, var(--mauve) 35%, transparent)); }
</style>
