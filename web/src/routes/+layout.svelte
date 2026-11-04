<script lang="ts">
	import '../app.css';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, errMsg, get } from '#lib/api.ts';
	import { live, connectEvents, disconnectEvents } from '#lib/live.svelte.ts';

	let { children } = $props();

	let meErr = $state('');
	let bare = $derived(page.url.pathname === '/login');
	const pwPath = '/account/password';

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
		{ href: '/', label: 'Applications', match: (p: string) => p === '/' || p.startsWith('/apps') },
		{ href: '/settings', label: 'Settings', match: (p: string) => p.startsWith('/settings') }
	];
</script>

{#if bare}
	{@render children()}
{:else if live.me}
	<div class="shell">
		<aside>
			<a class="brand" href="/"><span class="logo" aria-hidden="true"></span>dockercd</a>
			<nav aria-label="Main">
				{#each nav as n (n.href)}
					<a href={n.href} aria-current={n.match(page.url.pathname) ? 'page' : undefined}>{n.label}</a>
				{/each}
			</nav>
		</aside>
		<div class="main">
			<header>
				<span class="conn small" class:on={live.connected} title="Live updates">
					{live.connected ? 'Live' : 'Offline'}
				</span>
				<span class="spacer"></span>
				<a class="small muted" href={pwPath}>{live.me.username}</a>
				<button class="ghost" onclick={logout}>Log out</button>
			</header>
			<main>
				{@render children()}
			</main>
		</div>
	</div>
{:else if meErr}
	<p class="error" style="margin: 2rem">Cannot reach dockercd: {meErr}</p>
{/if}

<style>
	.shell { display: grid; grid-template-columns: 200px 1fr; min-height: 100vh; }
	aside { background: var(--mantle); border-right: 1px solid var(--surface0); padding: 1rem 0.7rem; }
	.brand { display: flex; align-items: center; gap: 0.5rem; font-weight: 700; color: var(--text); font-size: 1.05rem; padding: 0 0.5rem 1.2rem; }
	.brand:hover { text-decoration: none; }
	.logo { width: 18px; height: 18px; border-radius: 5px; background: var(--accent); }
	nav { display: grid; gap: 2px; }
	nav a { color: var(--subtext1); padding: 0.4rem 0.6rem; border-radius: var(--radius); }
	nav a:hover { background: var(--surface0); text-decoration: none; }
	nav a[aria-current='page'] { background: var(--surface0); color: var(--text); box-shadow: inset 2px 0 0 var(--accent); }
	.main { min-width: 0; display: flex; flex-direction: column; }
	header { display: flex; align-items: center; gap: 0.8rem; padding: 0.5rem 1.5rem; border-bottom: 1px solid var(--surface0); }
	main { padding: 1.5rem; max-width: 1200px; width: 100%; }
	.conn::before { content: '○ '; }
	.conn { color: var(--overlay1); }
	.conn.on { color: var(--green); }
	.conn.on::before { content: '● '; }
	@media (max-width: 720px) {
		.shell { grid-template-columns: 1fr; }
		aside { display: flex; align-items: center; gap: 1rem; padding: 0.5rem 1rem; border-right: 0; border-bottom: 1px solid var(--surface0); }
		.brand { padding: 0; }
		nav { display: flex; }
	}
</style>
