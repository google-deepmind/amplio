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
	// The chips of what a message (or a new run) carries: "Verify · 3 rounds",
	// "Repeat every 30m · until Nov 17", each opening its form, each removable;
	// and, where there's room, ghost chips that add an option (`adders`; a
	// composer card has its + menu instead). On a message, Verify with Repeat
	// verifies each tick ("Verify each tick · 3 rounds"). Each chip's button is
	// [data-chip=<option>], for the host to anchor the form to and focus.
	import { PlusIcon, RepeatIcon, SealCheckIcon, XIcon } from 'phosphor-svelte';
	import { serverSettings } from '$lib/serverSettings.svelte';
	import { loopChip, verifyChip } from '$lib/composerOptions';
	import type { ComposerState, Option } from '$lib/composer.svelte';

	let {
		composer,
		adders = null,
		dialog = false,
		onremove
	}: {
		composer: ComposerState;
		// Ghost chips that add an option not yet set; verify null = not offered.
		adders?: { verify: string | null; loop: string } | null;
		dialog?: boolean; // a chip opens its form in a dialog (a composer card's), not inline
		// An option was removed and its chip with it: the host's text box should take the focus.
		onremove?: () => void;
	} = $props();

	const zone = $derived(serverSettings.displayZone);
	// A Repeat chip's end ("until 16:00") counts from the send, so it moves on
	// with the clock until then, as the loop form's preview does.
	let now = $state(serverSettings.now());
	$effect(() => {
		if (composer.loop?.ends.kind !== 'after') return;
		now = serverSettings.now();
		const t = setInterval(() => (now = serverSettings.now()), 15_000);
		return () => clearInterval(t);
	});
	const addVerify = $derived(!!adders?.verify && !composer.verify);
	const addLoop = $derived(!!adders && !composer.loop);
	const eachTick = $derived(composer.verifiesEachTick);

	function toggle(which: Option) {
		composer.show(composer.open === which ? null : which);
	}
	function remove(which: Option) {
		if (which === 'verify') composer.verify = null;
		else composer.loop = composer.loopVerify = null;
		if (composer.open === which) composer.close();
		onremove?.();
	}
</script>

{#snippet adder(which: Option, label: string)}
	{@const why = composer.blocked(which)}
	<button
		type="button"
		class="adder"
		data-chip={which}
		aria-expanded={composer.open === which}
		aria-disabled={!!why}
		title={why || undefined}
		onclick={() => (why ? (composer.hint = why) : toggle(which))}
	>
		<PlusIcon size={12} weight="bold" />{label}
	</button>
{/snippet}

{#if composer.verify || composer.loop || addVerify || addLoop}
	<div class="chips">
		{#if composer.verify}
			<span class="chip" class:on={composer.open === 'verify'}>
				<button
					type="button"
					class="chip-main"
					data-chip="verify"
					aria-haspopup={dialog ? 'dialog' : undefined}
					aria-expanded={composer.open === 'verify'}
					title={composer.verify.criteria
						? `Criteria: ${composer.verify.criteria}`
						: eachTick
							? "Against each tick's prompt"
							: "Against the agent's current request"}
					onclick={() => toggle('verify')}
				>
					<SealCheckIcon size={13} weight="bold" />
					<span class="label">{verifyChip(composer.verify, serverSettings.verify.max_rounds, eachTick)}</span>
				</button>
				<button type="button" class="chip-x" aria-label="Remove verification" onclick={() => remove('verify')}
					><XIcon size={12} weight="bold" /></button
				>
			</span>
		{/if}
		{#if composer.loop}
			<span class="chip" class:on={composer.open === 'loop'}>
				<button
					type="button"
					class="chip-main"
					data-chip="loop"
					aria-haspopup={dialog ? 'dialog' : undefined}
					aria-expanded={composer.open === 'loop'}
					onclick={() => toggle('loop')}
				>
					<RepeatIcon size={13} weight="bold" />
					<span class="label">{loopChip(composer.loop, zone, now, composer.ownLoopVerify && !!composer.loopVerify)}</span>
				</button>
				<button type="button" class="chip-x" aria-label="Remove repeat" onclick={() => remove('loop')}
					><XIcon size={12} weight="bold" /></button
				>
			</span>
		{/if}
		{#if addVerify && adders?.verify}{@render adder('verify', adders.verify)}{/if}
		{#if addLoop && adders}{@render adder('loop', adders.loop)}{/if}
	</div>
{/if}

<style>
	.chips {
		display: flex;
		flex-wrap: wrap;
		gap: 0.35rem;
		min-width: 0;
	}
	.chip {
		display: inline-flex;
		align-items: stretch;
		max-width: 100%;
		border: 1px solid color-mix(in srgb, var(--accent) 45%, var(--border));
		border-radius: var(--radius-pill);
		background: color-mix(in srgb, var(--accent) 10%, transparent);
		color: var(--accent);
		font-size: var(--fs-sm);
		overflow: hidden;
	}
	.chip.on {
		background: color-mix(in srgb, var(--accent) 22%, transparent);
	}
	.chip button {
		background: none;
		border: none;
		border-radius: 0;
		color: inherit;
		min-height: 0;
	}
	.chip-main {
		display: inline-flex;
		align-items: center;
		gap: 0.3rem;
		padding: 0.15rem 0.3rem 0.15rem 0.6rem;
		min-width: 0;
		white-space: nowrap;
	}
	.chip-main > :global(svg) {
		flex-shrink: 0;
	}
	.label {
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	.chip-x {
		display: inline-flex;
		align-items: center;
		padding: 0.15rem 0.5rem 0.15rem 0.3rem;
		opacity: 0.75;
	}
	.chip-x:hover {
		opacity: 1;
	}
	.adder {
		display: inline-flex;
		align-items: center;
		gap: 0.3rem;
		padding: 0.15rem 0.6rem;
		min-height: 0;
		border: 1px dashed var(--border);
		border-radius: var(--radius-pill);
		background: none;
		color: var(--text-dim);
		font-size: var(--fs-sm);
	}
	.adder:hover:not([aria-disabled='true']),
	.adder[aria-expanded='true'] {
		color: var(--text);
		border-color: var(--accent-dim);
	}
	.adder[aria-disabled='true'] {
		opacity: 0.5;
		cursor: default;
	}
	@media (max-width: 640px) {
		/* Chips keep their compact height; the tap target is the whole pill. */
		.chip-main,
		.chip-x,
		.adder {
			min-height: var(--tap);
		}
	}
</style>
