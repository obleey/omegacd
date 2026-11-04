<script lang="ts">
	// OmegaCD mark: an Ω drawn as a path (not a font glyph) so it renders the same everywhere.
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
	<path
		d="M8 23.5h4.6a7.6 7.6 0 1 1 6.8 0H24"
		fill="none"
		stroke="var(--crust)"
		stroke-width="2.6"
		stroke-linecap="round"
		stroke-linejoin="round"
	/>
</svg>

<style>
	.logo { flex: none; display: block; filter: drop-shadow(0 4px 10px color-mix(in srgb, var(--mauve) 35%, transparent)); }
</style>
