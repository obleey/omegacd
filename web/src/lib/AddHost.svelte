<script lang="ts">
	// Add Host (M5-01, Q37, Q43): a name, the command that enrolls its Agent with a single-use invite, a live wait for
	// the Agent, its fingerprint compared, Approve.
	import { api, enc, errMsg, get } from './api.ts';
	import { agentCommand, composeAgent, httpWarning } from './addhost.ts';
	import Dialog from './Dialog.svelte';
	import { when } from './display.svelte.ts';
	import Icon from './Icon.svelte';
	import { live } from './live.svelte.ts';
	import { toast, toastError } from './toast.svelte.ts';

	let { open = $bindable(false) }: { open: boolean } = $props();

	let name = $state('');
	let invite = $state('');
	let expiresAt = $state('');
	let url = $state(location.origin); // Agents may reach OmegaCD at another name than this browser (Q160)
	let tab = $state<'cli' | 'docker'>('docker');
	let error = $state('');
	let busy = $state(false);
	let version = $state('');
	let now = $state(Date.now());

	$effect(() => {
		if (!open) return;
		name = invite = expiresAt = error = '';
		url = location.origin;
		get<{ version: string }>('/version')
			.then((r) => (version = r.version))
			.catch(() => {}); // the image is then :latest
		const t = setInterval(() => (now = Date.now()), 1000);
		return () => clearInterval(t);
	});

	let host = $derived(invite ? live.hosts[name] : undefined);
	let expired = $derived(!!expiresAt && !host?.fingerprint && now > new Date(expiresAt).getTime());
	let warning = $derived(httpWarning(url.trim()));
	let command = $derived(tab === 'cli' ? agentCommand(url.trim(), invite) : composeAgent(url.trim(), invite, version));

	async function getInvite(e?: SubmitEvent) {
		e?.preventDefault();
		error = '';
		busy = true;
		try {
			const r = await api<{ invite: string; expiresAt: string }>('POST', `/settings/hosts/${enc(name.trim())}/invite`);
			name = name.trim();
			invite = r.invite;
			expiresAt = r.expiresAt;
		} catch (err) {
			error = errMsg(err);
		} finally {
			busy = false;
		}
	}

	async function copy() {
		try {
			await navigator.clipboard.writeText(command);
			toast('green', 'Command copied', `Run it on ${name}`);
		} catch (err) {
			toastError(err, 'Copying the command failed');
		}
	}

	// Cancel: the Host isn't wanted after all; it goes with its invite, and the key of an Agent that enrolled meanwhile
	async function cancel() {
		busy = true;
		try {
			await api('DELETE', `/settings/hosts/${enc(name)}`);
			toast('green', `${name} cancelled`, 'Removed with its invite');
			open = false;
		} catch (err) {
			toastError(err, `Cancelling ${name} failed`);
		} finally {
			busy = false;
		}
	}

	async function approve() {
		if (!host?.fingerprint) return;
		busy = true;
		try {
			await api('POST', `/settings/hosts/${enc(name)}/approve`, { fingerprint: host.fingerprint });
		} catch (err) {
			toastError(err, `Approving ${name} failed`);
		} finally {
			busy = false;
		}
	}

</script>

<Dialog bind:open title={invite ? `Add Host ${name}` : 'Add Host'} wide>
	{#if !invite}
		<form id="addhost" onsubmit={getInvite}>
			<p class="muted small">The name Git references this machine by, in each Application's <code>host:</code>. Lowercase letters, digits,
				<code>-</code> and <code>_</code>.</p>
			<div class="field">
				<label for="ah-name">Name</label>
				<input id="ah-name" bind:value={name} required pattern="[a-z0-9][a-z0-9_-]{'{0,62}'}" placeholder="web-1" autocomplete="off" />
			</div>
			{#if error}<p class="error" role="alert">{error}</p>{/if}
		</form>
	{:else}
		<ol class="steps">
			<li>
				<h3>Run its Agent on {name}</h3>
				<div class="field">
					<label for="ah-url">OmegaCD's address, as the Host reaches it</label>
					<input id="ah-url" bind:value={url} autocomplete="off" />
				</div>
				{#if warning}<p class="warn small" role="alert"><Icon name="alert" size={14} />{warning}</p>{/if}
				<div class="seg" role="group" aria-label="How to run the Agent">
					<button type="button" aria-pressed={tab === 'docker'} onclick={() => (tab = 'docker')}><Icon name="container" size={15} />Docker Compose</button>
					<button type="button" aria-pressed={tab === 'cli'} onclick={() => (tab = 'cli')}><Icon name="terminal" size={15} />Command line</button>
				</div>
				<pre class="cmd">{command}</pre>
				<div class="row">
					<button type="button" class="primary" onclick={copy}><Icon name="file-text" size={15} />Copy</button>
					<span class="muted small">
						{#if tab === 'cli'}With <code>omegacd</code> installed on {name}; as a user in the <code>docker</code> group, or with rootless Docker or Podman.{:else}On {name}, as a user in the <code>docker</code> group; rootless Docker or Podman: the lines in <code>agent.yaml</code>'s comment. The invite is single-use and valid until {when(expiresAt, 'time')}.{/if}
					</span>
				</div>
			</li>
			<li aria-live="polite">
				<h3>Approve its key</h3>
				{#if host?.approved}
					<p class="ok"><Icon name="check-circle" size={16} />{name} is approved: Applications with <code>host: {name}</code> deploy to it.</p>
				{:else if host?.fingerprint}
					<p class="small">Its Agent enrolled with this key. Compare it with the fingerprint the Agent printed on {name}:</p>
					<code class="fp">{host.fingerprint}</code>
					<div class="row"><button type="button" class="primary" onclick={approve} disabled={busy}><Icon name="check" size={15} />Approve</button></div>
				{:else if expired}
					<p class="warn small"><Icon name="clock" size={14} />The invite expired before an Agent used it.</p>
					<div class="row"><button type="button" onclick={() => getInvite()} disabled={busy}>New invite</button></div>
				{:else}
					<p class="muted small waiting"><Icon name="notch" size={14} spin />Waiting for {name}'s Agent to connect…</p>
				{/if}
			</li>
		</ol>
	{/if}
	{#snippet actions()}
		{#if !invite}
			<button type="button" onclick={() => (open = false)}>Cancel</button>
			<button type="submit" form="addhost" class="primary" disabled={busy}>Next</button>
		{:else if host?.approved}
			<button type="button" class="primary" onclick={() => (open = false)}>Done</button>
		{:else}
			<button type="button" class="danger" onclick={cancel} disabled={busy} title="Remove {name} and its invite">Cancel</button>
			<button type="button" onclick={() => (open = false)} title="Keep {name} waiting for its Agent; its card has the rest">Close</button>
		{/if}
	{/snippet}
</Dialog>

<style>
	.steps { margin: 0; padding-left: 1.2rem; display: grid; gap: 1.2rem; }
	.steps > li { min-width: 0; } /* the command scrolls inside, not the dialog past a phone's width */
	h3 { margin: 0 0 0.6rem; font-size: 0.95rem; }
	.cmd { background: var(--crust); padding: 0.6rem 0.8rem; border-radius: var(--radius); overflow-x: auto; margin: 0.6rem 0; font-size: 0.82rem; }
	.seg { margin-top: 0.4rem; }
	.fp { display: block; background: var(--crust); padding: 0.4rem 0.6rem; border-radius: 4px; margin: 0.4rem 0 0.6rem; word-break: break-all; }
	.warn, .ok, .waiting { display: flex; align-items: center; gap: 0.4rem; }
	.warn { color: var(--yellow); }
	.ok { color: var(--green); }
</style>
