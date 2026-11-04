<script lang="ts">
	import '../app.css';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, errMsg, get } from '#lib/api.ts';
	import { live, connectEvents, disconnectEvents, loadApps } from '#lib/live.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import Logo from '#lib/Logo.svelte';
	import ThemeToggle from '#lib/ThemeToggle.svelte';
	import Version from '#lib/Version.svelte';
	import Toasts from '#lib/Toasts.svelte';

	let { children } = $props();

	let meErr = $state('');
	let bare = $derived(page.url.pathname === '/login');
	const pwPath = '/account/password';
	// Any Sync Operation running anywhere drives the top progress bar.
	let anyRunning = $derived(Object.values(live.apps).some((a) => a.lastOperation?.phase === 'Running'));

	$effect(() => {
		if (bare) return;
		if (!live.me) {
			get<{ username: string; mustChangePassword: boolean }>('/auth/me')
				.then((me) => (live.me = me))
				.catch((e) => (meErr = errMsg(e)));
			return;
		}
		if (live.me.mustChangePassword) {
			if (page.url.pathname !== pwPath) goto(pwPath);
			return;
		}
		connectEvents();
		// Every page (tiles, App of Apps children, the top progress bar) reads the shared store.
		if (!live.loaded) loadApps().catch(() => {});
	});

	async function logout() {
		try {
			await api('POST', '/auth/logout');
		} catch {
			/* go to login regardless */
		}
		disconnectEvents();
		live.me = null;
		goto('/login');
	}

	const nav = [
		{ href: '/', label: 'Applications', icon: 'app', match: (p: string) => p === '/' || p.startsWith('/apps') },
		{ href: '/settings', label: 'Settings', icon: 'settings', match: (p: string) => p.startsWith('/settings') }
	] as const;
</script>

{#if anyRunning && !bare}<div class="topbar" role="progressbar" aria-label="Sync Operation running"></div>{/if}

{#if bare}
	{@render children()}
{:else if live.me}
	<div class="shell">
		<aside>
			<a class="brand" href="/"><Logo size={30} /><span>OmegaCD</span></a>
			<nav aria-label="Main">
				{#each nav as n (n.href)}
					<a href={n.href} aria-current={n.match(page.url.pathname) ? 'page' : undefined}
						><Icon name={n.icon} size={17} /><span>{n.label}</span></a
					>
				{/each}
			</nav>
			<div class="ver"><Version /></div>
			<div class="foot">
				<span class="conn small" class:on={live.connected} title={live.connected ? 'Receiving live updates' : 'Live updates disconnected'}>
					<span class="dot" aria-hidden="true"></span><span class="lbl">{live.connected ? 'Live' : 'Offline'}</span>
				</span>
				<a class="user small" href={pwPath} title="Change password"><Icon name="user" size={15} /><span class="lbl">{live.me.username}</span></a>
				<ThemeToggle />
				<button class="ghost sm icon-btn" onclick={logout} title="Log out" aria-label="Log out"><Icon name="logout" size={15} /></button>
			</div>
		</aside>
		<div class="main">
			<main>
				{@render children()}
			</main>
		</div>
	</div>
{:else if meErr}
	<p class="error" style="margin: 2rem">Cannot reach OmegaCD: {meErr}</p>
{/if}

<Toasts />

<style>
	.shell { display: grid; grid-template-columns: 220px minmax(0, 1fr); min-height: 100vh; background: linear-gradient(to right, var(--mantle) 219px, var(--border) 219px 220px, transparent 220px); }
	aside { position: sticky; top: 0; height: 100vh; display: flex; flex-direction: column; gap: 0.3rem; padding: 1.1rem 0.8rem 0.8rem; }
	.brand { display: flex; align-items: center; gap: 0.6rem; font-weight: 700; color: var(--text); font-size: 1.12rem; letter-spacing: -0.02em; padding: 0 0.45rem 1.3rem; }
	.brand:hover { text-decoration: none; }
	nav { display: grid; gap: 3px; }
	nav a { display: flex; align-items: center; gap: 0.65rem; color: var(--subtext1); padding: 0.5rem 0.7rem; border-radius: var(--radius); font-weight: 520; transition: background-color 0.15s, color 0.15s; position: relative; }
	nav a:hover { background: color-mix(in srgb, var(--surface0) 60%, transparent); text-decoration: none; color: var(--text); }
	nav a[aria-current='page'] { background: color-mix(in srgb, var(--accent) 14%, transparent); color: var(--accent); }
	nav a[aria-current='page']::before { content: ''; position: absolute; left: -0.8rem; top: 20%; bottom: 20%; width: 3px; border-radius: 0 3px 3px 0; background: var(--accent); }
	.foot { margin-top: 0; display: flex; align-items: center; gap: 0.3rem; flex-wrap: wrap; padding-top: 0.7rem; border-top: 1px solid var(--border); }
	.conn { display: flex; align-items: center; gap: 0.5rem; color: var(--overlay1); padding: 0.3rem 0.5rem; width: 100%; }
	.dot { width: 8px; height: 8px; border-radius: 50%; background: var(--overlay0); position: relative; flex: none; }
	.conn.on { color: var(--green); }
	.conn.on .dot { background: var(--green); }
	.conn.on .dot::after { content: ''; position: absolute; inset: 0; border-radius: 50%; background: var(--green); animation: ping 2s var(--ease) infinite; }
	@keyframes ping { 0% { transform: scale(1); opacity: 0.6; } 80%, 100% { transform: scale(2.8); opacity: 0; } }
	.user { flex: 1; min-width: 0; display: inline-flex; align-items: center; gap: 0.45rem; color: var(--subtext1); padding: 0.35rem 0.5rem; border-radius: var(--radius); }
	.user:hover { background: color-mix(in srgb, var(--surface0) 60%, transparent); text-decoration: none; }
	.foot button { color: var(--subtext0); }
	.main { min-width: 0; display: flex; flex-direction: column; }
	main { padding: 1.8rem 2rem 4rem; max-width: 1480px; width: 100%; }
	.topbar { position: fixed; top: 0; left: 0; right: 0; height: 3px; z-index: 60; overflow: hidden; background: color-mix(in srgb, var(--blue) 20%, transparent); }
	.topbar::before { content: ''; position: absolute; top: 0; bottom: 0; width: 40%; background: linear-gradient(90deg, transparent, var(--blue), var(--mauve), transparent); animation: indet 1.3s var(--ease) infinite; }
	@keyframes indet { from { left: -40%; } to { left: 100%; } }
	@media (max-width: 760px) {
		.shell { grid-template-columns: 1fr; background: none; }
		aside { z-index: 20; height: auto; flex-direction: row; align-items: center; gap: 0.3rem; padding: 0.5rem 0.75rem; background: color-mix(in srgb, var(--mantle) 92%, transparent); backdrop-filter: blur(8px); border-bottom: 1px solid var(--border); }
		.brand { padding: 0 0.3rem 0 0; }
		.brand span { display: none; }
		nav { display: flex; }
		nav a { padding: 0.4rem 0.6rem; }
		nav a[aria-current='page']::before { display: none; }
		.foot { margin: 0 0 0 auto; padding: 0; border: 0; flex-wrap: nowrap; }
		.ver { display: none; }
		.conn { width: auto; padding: 0.3rem; }
		.lbl { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; }
		.user { flex: none; padding: 0.4rem; }
		main { padding: 1.1rem 1rem 4rem; }
	}
	@media (max-width: 400px) {
		nav a span { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; }
	}
	.ver { margin-top: auto; padding: 0 0.6rem 0.4rem; }
</style>
