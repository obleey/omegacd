<script lang="ts" module>
	// Stroke icons on a 24×24 grid (lucide-style), inlined so no icon font or request is needed.
	const C = 'M2 12a10 10 0 1 0 20 0a10 10 0 1 0-20 0'; // circle r10
	const HEART = 'M19 14c1.5-1.5 3-3.2 3-5.5A5.5 5.5 0 0 0 16.5 3c-1.8 0-3 .5-4.5 2-1.5-1.5-2.7-2-4.5-2A5.5 5.5 0 0 0 2 8.5c0 2.3 1.5 4 3 5.5l7 7Z';
	const FILE = 'M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8Z M14 2v6h6';
	export const icons = {
		'check-circle': `${C} M8 12.5l2.8 2.8L16 9.5`,
		'arrow-circle': `${C} M12 16.5V7.5 M8 11.5l4-4 4 4`,
		question: `${C} M9.1 9a3 3 0 0 1 5.8 1c0 2-3 3-3 3 M12 17h.01`,
		sync: 'M3 12a9 9 0 0 1 15.4-6.4L21 8 M21 3v5h-5 M21 12a9 9 0 0 1-15.4 6.4L3 16 M8 16H3v5',
		heart: HEART,
		'heart-broken': `${HEART} M12 5l-1.5 3.5 3 2-2 3.5`,
		notch: 'M21 12a9 9 0 1 1-6.2-8.56',
		ghost: 'M9 10h.01 M15 10h.01 M12 2a8 8 0 0 0-8 8v12l3-3 2.5 2.5L12 19l2.5 2.5L17 19l3 3V10a8 8 0 0 0-8-8Z',
		'x-circle': `${C} M15 9l-6 6 M9 9l6 6`,
		'stop-circle': `${C} M9.5 9.5h5v5h-5Z`,
		alert: 'M21.7 18l-8-14a2 2 0 0 0-3.4 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.7-3Z M12 9v4 M12 17h.01',
		info: `${C} M12 16v-4 M12 8h.01`,
		check: 'M20 6 9 17l-5-5',
		x: 'M18 6 6 18 M6 6l12 12',
		app: 'M4 3h6a1 1 0 0 1 1 1v6a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1Z M14 3h6a1 1 0 0 1 1 1v6a1 1 0 0 1-1 1h-6a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1Z M14 13h6a1 1 0 0 1 1 1v6a1 1 0 0 1-1 1h-6a1 1 0 0 1-1-1v-6a1 1 0 0 1 1-1Z M4 13h6a1 1 0 0 1 1 1v6a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1v-6a1 1 0 0 1 1-1Z',
		apps: 'M12 2 2 7l10 5 10-5-10-5Z M2 17l10 5 10-5 M2 12l10 5 10-5',
		service: 'M12 2l8.7 5v10L12 22l-8.7-5V7Z M12 22V12 M20.7 7 12 12 3.3 7',
		container: 'M21 8a2 2 0 0 0-1-1.7l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.7l7 4a2 2 0 0 0 2 0l7-4a2 2 0 0 0 1-1.7Z M3.3 7 12 12l8.7-5 M12 22V12 M7.5 4.3l9 5.2',
		volume: 'M3 5a9 3 0 1 0 18 0a9 3 0 1 0-18 0 M3 5v14a9 3 0 0 0 18 0V5 M3 12a9 3 0 0 0 18 0',
		network: 'M17 16h4a1 1 0 0 1 1 1v4a1 1 0 0 1-1 1h-4a1 1 0 0 1-1-1v-4a1 1 0 0 1 1-1Z M3 16h4a1 1 0 0 1 1 1v4a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1v-4a1 1 0 0 1 1-1Z M10 2h4a1 1 0 0 1 1 1v4a1 1 0 0 1-1 1h-4a1 1 0 0 1-1-1V3a1 1 0 0 1 1-1Z M5 16v-3a1 1 0 0 1 1-1h12a1 1 0 0 1 1 1v3 M12 12V8',
		file: FILE,
		'file-text': `${FILE} M8 13h8 M8 17h5`,
		'file-cog': `${FILE} M10 15h4 M12 13v4`,
		lock: 'M6 11h12a1 1 0 0 1 1 1v8a1 1 0 0 1-1 1H6a1 1 0 0 1-1-1v-8a1 1 0 0 1 1-1Z M8 11V7a4 4 0 0 1 8 0v4',
		folder: 'M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.7-.9l-.8-1.2A2 2 0 0 0 7.9 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z',
		branch: 'M6 3v12 M18 9a3 3 0 1 0 0-6a3 3 0 0 0 0 6Z M6 21a3 3 0 1 0 0-6a3 3 0 0 0 0 6Z M18 9a9 9 0 0 1-9 9',
		commit: 'M9 12a3 3 0 1 0 6 0a3 3 0 1 0-6 0 M3 12h6 M15 12h6',
		server: 'M4 2h16a2 2 0 0 1 2 2v4a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2Z M4 14h16a2 2 0 0 1 2 2v4a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2v-4a2 2 0 0 1 2-2Z M6 6h.01 M6 18h.01',
		refresh: 'M21 12a9 9 0 1 1-9-9c2.5 0 4.9 1 6.7 2.7L21 8 M21 3v5h-5',
		grid: 'M4 3h6a1 1 0 0 1 1 1v6a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1Z M14 3h6a1 1 0 0 1 1 1v6a1 1 0 0 1-1 1h-6a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1Z M14 13h6a1 1 0 0 1 1 1v6a1 1 0 0 1-1 1h-6a1 1 0 0 1-1-1v-6a1 1 0 0 1 1-1Z M4 13h6a1 1 0 0 1 1 1v6a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1v-6a1 1 0 0 1 1-1Z',
		list: 'M8 6h13 M8 12h13 M8 18h13 M3 6h.01 M3 12h.01 M3 18h.01',
		tree: 'M3 4h6v5H3Z M15 3h6v4h-6Z M15 10h6v4h-6Z M15 17h6v4h-6Z M9 6.5h3v12h3 M12 5h3 M12 12h3',
		chevron: 'm9 18 6-6-6-6',
		plus: 'M5 12h14 M12 5v14',
		search: 'M11 19a8 8 0 1 0 0-16a8 8 0 0 0 0 16Z M21 21l-4.3-4.3',
		logout: 'M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4 M16 17l5-5-5-5 M21 12H9',
		settings: 'M4 21v-7 M4 10V3 M12 21v-9 M12 8V3 M20 21v-5 M20 12V3 M1 14h6 M9 8h6 M17 16h6',
		trash: 'M3 6h18 M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6 M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2',
		edit: 'M17 3a2.85 2.85 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5Z',
		download: 'M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4 M7 10l5 5 5-5 M12 15V3',
		clock: `${C} M12 6v6l4 2`,
		undo: 'M3 7v6h6 M21 17a9 9 0 0 0-15-6.7L3 13',
		terminal: 'm4 17 6-6-6-6 M12 19h8',
		diff: 'M12 3v14 M5 10h14 M5 21h14',
		user: 'M19 21v-2a4 4 0 0 0-4-4H9a4 4 0 0 0-4 4v2 M12 11a4 4 0 1 0 0-8a4 4 0 0 0 0 8Z',
		key: 'M15.5 7.5l2.3 2.3a1 1 0 0 0 1.4 0l2.1-2.1a1 1 0 0 0 0-1.4L19 4 M21 2l-9.6 9.6 M7.5 22a5.5 5.5 0 1 0 0-11a5.5 5.5 0 0 0 0 11Z',
		token: 'M2 12a10 10 0 1 0 20 0a10 10 0 1 0-20 0 M8 12h8 M12 8v8'
	} as const;
	export type IconName = keyof typeof icons;
</script>

<script lang="ts">
	// Decorative by default; pass `title` to make the icon itself announce something.
	let { name, size = 16, spin = false, title, class: cls = '' }: { name: IconName; size?: number; spin?: boolean; title?: string; class?: string } =
		$props();
</script>

<svg
	class="icon {cls}"
	class:spin
	width={size}
	height={size}
	viewBox="0 0 24 24"
	fill="none"
	stroke="currentColor"
	stroke-width="2"
	stroke-linecap="round"
	stroke-linejoin="round"
	role={title ? 'img' : undefined}
	aria-hidden={title ? undefined : 'true'}
	aria-label={title}
>
	<path d={icons[name]} />
</svg>

<style>
	.icon { flex: none; display: inline-block; vertical-align: -0.15em; }
	.spin { animation: spin 1.1s linear infinite; transform-origin: 50% 50%; }
	@keyframes spin { to { transform: rotate(360deg); } }
</style>
