<!--
 Copyright 2026 Google LLC

 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
-->

<script lang="ts">
	import { tick } from 'svelte';
	import { DeviceMobileIcon } from 'phosphor-svelte';
	import { beforeNavigate } from '$app/navigation';
	import { api, errorText } from '$lib/api';
	import { popover } from '$lib/dismiss';

	// "Open on phone": a QR of this origin plus a one-time login code, so a phone
	// camera opens Amplio already signed in (hooks.client.ts redeems ?pair=).
	// A code is minted on open and on "New code"; a replaced or closed code is
	// just forgotten here (the server drops it at expiry).

	const LOCAL_HOSTS = new Set(['localhost', '127.0.0.1', '[::1]']);
	const QR_PX = 176;

	let open = $state(false);
	let wrap = $state<HTMLElement>();
	let dialog = $state<HTMLElement>();
	let qr = $state(''); // data: URL of the QR image
	let qrPx = $state(0); // its CSS size: half its pixel size, so 2 device px per pixel
	let deadline = $state(0); // client-clock ms when the code expires
	let now = $state(Date.now());
	let error = $state('');
	let loading = $state(false);
	let gen = 0; // bumps on every mint/close, so a stale response is dropped

	const local = $derived(open && LOCAL_HOSTS.has(location.hostname));
	const left = $derived(Math.max(0, Math.ceil((deadline - now) / 1000)));
	const expired = $derived(!!qr && left === 0);
	const clock = $derived(`${Math.floor(left / 60)}:${String(left % 60).padStart(2, '0')}`);

	const pop = popover(() => close());
	beforeNavigate(() => close());
	$effect(() => () => void pop.closed());

	$effect(() => {
		if (!qr || expired) return;
		const id = setInterval(() => (now = Date.now()), 1000);
		return () => clearInterval(id);
	});

	// A PNG with whole device pixels per module at 2x, shown at half size so
	// modules stay sharp.
	function qrImage(qrcode: typeof import('qrcode-generator').default, text: string) {
		const qr = qrcode(0, 'M');
		qr.addData(text);
		qr.make();
		const n = qr.getModuleCount();
		const quiet = 2; // the white .qr box around the image adds the rest
		const cell = Math.max(1, Math.floor((QR_PX * 2) / (n + 2 * quiet)));
		const canvas = document.createElement('canvas');
		canvas.width = canvas.height = cell * (n + 2 * quiet);
		const ctx = canvas.getContext('2d')!;
		ctx.fillStyle = '#fff';
		ctx.fillRect(0, 0, canvas.width, canvas.height);
		ctx.fillStyle = '#000';
		for (let r = 0; r < n; r++)
			for (let c = 0; c < n; c++)
				if (qr.isDark(r, c)) ctx.fillRect((c + quiet) * cell, (r + quiet) * cell, cell, cell);
		return { src: canvas.toDataURL('image/png'), px: canvas.width / 2 };
	}

	async function mint() {
		const mine = ++gen;
		loading = true;
		error = '';
		qr = '';
		try {
			// Loaded on first open only: keeps the encoder out of the main bundle.
			const [{ code, expires_in }, { default: qrcode }] = await Promise.all([
				api.mintPairCode(),
				import('qrcode-generator')
			]);
			const img = qrImage(qrcode, `${location.origin}/?pair=${encodeURIComponent(code)}`);
			if (mine !== gen) return;
			deadline = Date.now() + expires_in * 1000;
			now = Date.now();
			qr = img.src;
			qrPx = img.px;
		} catch (e) {
			if (mine === gen) error = errorText(e) || 'Server unreachable.';
		} finally {
			if (mine === gen) loading = false;
		}
	}

	async function show() {
		open = true;
		if (wrap) pop.opened(wrap);
		if (!LOCAL_HOSTS.has(location.hostname)) mint();
		await tick();
		dialog?.focus();
	}

	function close() {
		if (!open) return;
		open = false;
		gen++;
		qr = '';
		error = '';
		loading = false;
		pop.closed();
	}
</script>

<div class="wrap" bind:this={wrap}>
	<button
		class="trigger"
		aria-label="Open on phone"
		title="Open on phone"
		aria-haspopup="dialog"
		aria-expanded={open}
		onclick={() => (open ? close() : show())}
	>
		<DeviceMobileIcon size={13} weight="bold" />
	</button>
	{#if open}
		<div class="pop" role="dialog" aria-label="Open on phone" tabindex="-1" bind:this={dialog}>
			{#if local}
				<p class="warn">
					Your phone can't reach localhost. Open Amplio through this machine's hostname, then
					try again.
				</p>
			{:else}
				<div class="qr" class:dead={expired}>
					{#if qr && !expired}
						<img src={qr} alt="QR code that opens Amplio on your phone" width={qrPx} height={qrPx} />
					{/if}
				</div>
				{#if error}
					<p class="msg err" role="alert">{error}</p>
					<button onclick={mint}>Try again</button>
				{:else if expired}
					<p class="msg" role="status">This code expired.</p>
					<button onclick={mint}>New code</button>
				{:else if loading}
					<p class="msg">Creating a code…</p>
				{:else}
					<p class="msg">Scan with your phone's camera. Works once, expires in {clock}.</p>
					<button class="link" onclick={mint}>New code</button>
				{/if}
			{/if}
		</div>
	{/if}
</div>

<style>
	/* Stretched to the row, so the trigger is exactly as tall as the chips. */
	.wrap {
		position: relative;
		display: inline-flex;
		align-self: stretch;
	}
	/* Outlined like the stat chips it sits beside. */
	.trigger {
		display: inline-flex;
		align-items: center;
		padding: 0 0.5rem;
		border-radius: var(--radius-pill);
		border: 1px solid var(--border);
		background: color-mix(in srgb, var(--bg-elev2) 60%, transparent);
		color: var(--text-dim);
		cursor: pointer;
	}
	.trigger:hover,
	.trigger[aria-expanded='true'] {
		color: var(--text);
	}
	.pop {
		position: absolute;
		right: 0;
		top: calc(100% + 0.5rem);
		z-index: 20;
		width: 15rem;
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: 0.6rem;
		padding: 0.9rem;
		background: var(--bg-elev);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		box-shadow: 0 4px 12px rgba(0, 0, 0, 0.25);
		font-size: var(--fs-sm);
		color: var(--text);
	}
	.pop:focus {
		outline: none;
	}
	/* Cameras want dark modules on a light ground, whatever the theme. */
	.qr {
		width: 176px;
		height: 176px;
		border-radius: var(--radius-sm);
		display: grid;
		place-items: center;
		background: #fff;
		overflow: hidden;
	}
	.qr.dead {
		background: var(--bg-elev2);
	}
	.qr img {
		display: block;
	}
	/* Quiet, always-available refresh; the expired state gets a real button. */
	.link {
		margin-top: -0.3rem;
		padding: 0.1rem 0.3rem;
		border: none;
		background: none;
		color: var(--text-dim);
		font-size: var(--fs-xs);
		text-decoration: underline;
		text-underline-offset: 2px;
	}
	.link:hover:not(:disabled) {
		color: var(--accent);
	}
	.msg,
	.warn {
		margin: 0;
		text-align: center;
		line-height: 1.45;
		color: var(--text-dim);
	}
	.warn {
		color: var(--warn);
		text-align: left;
	}
	.err {
		color: var(--err);
	}
</style>
