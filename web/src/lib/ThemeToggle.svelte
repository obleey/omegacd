<script lang="ts">
	// Cycles System → Light → Dark. System follows prefers-color-scheme (no data-theme attribute);
	// app.html applies the stored choice before first paint.
	type Mode = 'system' | 'light' | 'dark';
	const KEY = 'omegacd.theme';
	const next: Record<Mode, Mode> = { system: 'light', light: 'dark', dark: 'system' };
	const label: Record<Mode, string> = { system: 'Theme: follows system', light: 'Theme: light', dark: 'Theme: dark' };

	let mode = $state<Mode>(read());

	function read(): Mode {
		try {
			const v = localStorage.getItem(KEY);
			return v === 'light' || v === 'dark' ? v : 'system';
		} catch {
			return 'system';
		}
	}

	function apply(m: Mode) {
		const root = document.documentElement;
		if (m === 'system') root.removeAttribute('data-theme');
		else root.setAttribute('data-theme', m === 'light' ? 'latte' : 'mocha');
		try {
			if (m === 'system') localStorage.removeItem(KEY);
			else localStorage.setItem(KEY, m);
		} catch {
			// private mode or blocked storage: the choice just isn't remembered
		}
	}

	function cycle() {
		mode = next[mode];
		apply(mode);
	}
</script>

<button class="ghost sm icon-btn" onclick={cycle} title="{label[mode]} (click to change)" aria-label="{label[mode]}, click to change">
	<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
		{#if mode === 'light'}
			<circle cx="12" cy="12" r="4" />
			<path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
		{:else if mode === 'dark'}
			<path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8Z" />
		{:else}
			<rect x="3" y="4" width="18" height="12" rx="2" />
			<path d="M8 20h8M12 16v4" />
		{/if}
	</svg>
</button>
