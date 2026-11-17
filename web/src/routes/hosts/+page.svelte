<script lang="ts">
	// Hosts (Q152): every machine Applications deploy to, through its Agent; added here (M5-01), kept live (Q153).
	import { onMount } from 'svelte';
	import { api, enc, errMsg, get, type HostKey } from '#lib/api.ts';
	import AddHost from '#lib/AddHost.svelte';
	import Badge from '#lib/Badge.svelte';
	import Dialog from '#lib/Dialog.svelte';
	import Empty from '#lib/Empty.svelte';
	import { shortKey, sopsYaml } from '#lib/hostkeys.ts';
	import Icon from '#lib/Icon.svelte';
	import { live, loadHosts } from '#lib/live.svelte.ts';
	import { toast, toastError } from '#lib/toast.svelte.ts';

	let error = $state('');
	let adding = $state(false);
	onMount(() => {
		loadHosts().catch((e) => (error = errMsg(e)));
	});
	let hosts = $derived(Object.values(live.hosts).sort((a, b) => a.name.localeCompare(b.name)));
	let allSops = $derived(sopsYaml(hosts.map((h) => ({ name: h.name, recipient: h.ageRecipient ?? '' }))));

	async function copyText(text: string, what: string) {
		try {
			await navigator.clipboard.writeText(text);
			toast('green', `${what} copied`);
		} catch (e) {
			toastError(e, `Copying ${what} failed`);
		}
	}
	async function delHost(name: string) {
		if (!confirm(`Delete Host “${name}”? Its Agent's key is revoked and its connection closed.`)) return;
		try {
			await api('DELETE', `/settings/hosts/${enc(name)}`);
		} catch (e) {
			toastError(e, `Deleting ${name} failed`);
		}
	}

	// Host Keys dialog: a Host's Host Key is born on its Agent; only its public half reaches the Control Plane (Q108).
	let keysOpen = $state(false);
	let keysHost = $state('');
	let keys = $state<HostKey[] | null>(null);
	let current = $derived(keys?.find((k) => k.current));
	async function openKeys(name: string) {
		keysHost = name;
		keys = null;
		keysOpen = true;
		try {
			keys = (await get<HostKey[]>(`/settings/hosts/${enc(name)}/keys`)) ?? [];
		} catch (e) {
			toastError(e, `Loading the keys of ${name} failed`);
		}
	}
</script>

<svelte:head><title>Hosts · OmegaCD</title></svelte:head>

<div class="head">
	<div>
		<h1>Hosts</h1>
		<p class="muted small sub">Machines Applications deploy to, each through its Agent. Each has its own Host Key, born on its Agent, which
			decrypts the Encrypted Secrets of its Applications: encrypt them for its public key in <code>.sops.yaml</code>.</p>
	</div>
	<button class="primary" onclick={() => (adding = true)}><Icon name="plus" size={15} />Add Host</button>
</div>
{#if error}<p class="error" role="alert">{error}</p>{/if}

{#if live.hostsLoaded && !hosts.length}
	<Empty icon="server" title="No Hosts yet">
		<p>Add the first machine: OmegaCD gives you the command that enrolls its Agent.</p>
	</Empty>
{:else}
	<section class="card">
		<div class="row tools"><button onclick={() => copyText(allSops, '.sops.yaml')}>Copy .sops.yaml for all Hosts</button></div>
		<div class="scroll-x"><table>
			<thead><tr><th>Name</th><th>Agent</th><th>Connection</th><th>Host Key</th><th>Applications</th><th><span class="sr-only">Actions</span></th></tr></thead>
			<tbody>
				{#each hosts as h (h.name)}
					<tr>
						<td class="name">{h.name}</td>
						<td class="small">
							{#if !h.fingerprint}invited{:else if h.locked?.length}locked: two connections{:else if h.approved}approved{:else}to approve{/if}
							{#if h.agent}<div class="muted small">Agent {h.agent.version}</div>{/if}
						</td>
						<td>
							<Badge status={h.status.connected ? 'Connected' : 'Disconnected'} label="Connection" />
							<div class="muted small">{h.status.connected ? `Engine ${h.status.serverVersion ?? ''}` : h.status.error}</div>
						</td>
						<td>
							{#if h.ageRecipient}
								<span class="keycell">
									<code title={h.ageRecipient}>{shortKey(h.ageRecipient)}</code>
									<button class="ghost sm" onclick={() => copyText(h.ageRecipient ?? '', `Public key of ${h.name}`)} aria-label="Copy the public key of {h.name}">Copy</button>
								</span>
							{:else}
								<span class="muted small">when its Agent enrolls</span>
							{/if}
						</td>
						<td>{h.apps.join(', ') || '—'}</td>
						<td class="right">
							<button onclick={() => openKeys(h.name)} aria-label="Keys of {h.name}">Keys</button>
							<span title={h.apps.length ? 'Used by Applications; delete or move them first' : undefined}>
								<button class="danger" onclick={() => delHost(h.name)} disabled={h.apps.length > 0}>Delete</button>
							</span>
						</td>
					</tr>
				{/each}
			</tbody>
		</table></div>
	</section>
{/if}

<AddHost bind:open={adding} />

<Dialog bind:open={keysOpen} title="Host Key of {keysHost}" wide>
	{#if keys === null}
		<p class="muted small" role="status">Loading…</p>
	{:else if current}
		<h3 class="dh">Public key</h3>
		<div class="row keyrow">
			<code class="tok">{current.recipient}</code>
			<button onclick={() => copyText(current!.recipient, 'Public key')}>Copy</button>
		</div>
		<h3 class="dh">.sops.yaml rule</h3>
		<pre class="tok">{sopsYaml([{ name: keysHost, recipient: current.recipient }])}</pre>
		<div class="row"><button onclick={() => copyText(sopsYaml([{ name: keysHost, recipient: current!.recipient }]), '.sops.yaml rule')}>Copy rule</button></div>
	{:else}
		<p class="muted small">No Host Key yet: the Host's Agent makes one when it enrolls. Its private half never leaves the Host.</p>
	{/if}
	{#snippet actions()}
		<button onclick={() => (keysOpen = false)}>Close</button>
	{/snippet}
</Dialog>

<style>
	.head { display: flex; align-items: flex-start; justify-content: space-between; gap: 1rem; flex-wrap: wrap; }
	.sub { margin: 0.15rem 0 1.2rem; max-width: 70ch; }
	tbody tr:hover { background: color-mix(in srgb, var(--surface0) 35%, transparent); }
	.right, .name { text-align: right; white-space: nowrap; }
	.name { text-align: left; }
	.tools { margin-bottom: 0.6rem; }
	.keycell { display: inline-flex; align-items: center; gap: 0.3rem; white-space: nowrap; }
	button.sm { padding: 0.1rem 0.45rem; font-size: 0.78rem; }
	.tok { background: var(--crust); padding: 0.3rem 0.5rem; border-radius: 4px; word-break: break-all; flex: 1; }
	pre.tok { margin: 0 0 0.5rem; white-space: pre-wrap; }
	.dh { margin: 1rem 0 0.4rem; font-size: 0.95rem; }
	.keyrow { flex-wrap: nowrap; }
</style>
