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
	// What the run being started carries, inline on the new-run form (a page of
	// its own, with room for its forms): the chips and the ghost chips that add
	// an option (OptionChips), the warning that goes with them, and the open
	// form below them. A composer card (MessageBox) opens its forms from its
	// toolbar instead. The state lives in a ComposerState the host sends from.
	// Why something was refused (composer.hint) shows below the chips, where
	// the forms open.
	import { tick } from 'svelte';
	import OptionChips from './OptionChips.svelte';
	import OptionForm from './OptionForm.svelte';
	import { WarningCircleIcon } from 'phosphor-svelte';
	import { serverSettings } from '$lib/serverSettings.svelte';
	import { optionsNote } from '$lib/composerOptions';
	import type { ComposerState, Option } from '$lib/composer.svelte';

	let {
		composer,
		withPrompt = false,
		promptPlaceholder,
		withSendNow = true,
		loopTitle,
		adders = null,
		onclose
	}: {
		composer: ComposerState;
		withPrompt?: boolean; // the loop has its own prompt (composer.loopPrompt), not the message
		promptPlaceholder?: string;
		withSendNow?: boolean;
		loopTitle?: string;
		// Ghost chips that add an option not yet set; verify null = not offered.
		adders?: { verify: string | null; loop: string } | null;
		// A form closed and no chip is left to take the focus: the host's text box should.
		onclose?: () => void;
	} = $props();

	const note = $derived(optionsNote(composer, serverSettings.verify.max_rounds));
	let root = $state<HTMLElement>();

	// After a form closes, the focus goes back where the operator was: its chip,
	// else the host's text box.
	async function closed(which: Option) {
		composer.close();
		await tick();
		const chip = root?.querySelector<HTMLElement>(`[data-chip="${which}"]`);
		if (chip) chip.focus();
		else onclose?.();
	}
</script>

{#if composer.verify || composer.loop || composer.open || adders}
	<div class="opts" bind:this={root}>
		<OptionChips {composer} {adders} onremove={onclose} />
		<!-- Why something was refused, where the forms open: here, below the chips. -->
		{#if composer.hint}
			<p class="hint small" role="status" aria-live="polite">
				<WarningCircleIcon size={16} weight="bold" /><span>{composer.hint}</span>
			</p>
		{/if}
		{#if note}<p class="cost small">{note}</p>{/if}
		{#if composer.open}
			<div class="panel">
				<OptionForm {composer} {withPrompt} {promptPlaceholder} {withSendNow} {loopTitle} ondone={closed} />
			</div>
		{/if}
	</div>
{/if}

<style>
	.opts {
		display: flex;
		flex-direction: column;
		gap: 0.45rem;
	}
	.cost {
		margin: 0;
		color: var(--warn);
	}
	.hint {
		display: flex;
		align-items: flex-start;
		gap: 0.45rem;
		margin: 0;
	}
	.hint :global(svg) {
		flex-shrink: 0;
		margin-top: 0.1rem;
		color: var(--warn);
	}
	.panel {
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-elev);
		padding: 0.6rem 0.75rem;
	}
	@media (max-width: 640px) {
		/* Open with the keyboard up: the transcript keeps some room. */
		.panel {
			max-height: 55dvh;
			overflow-y: auto;
			overscroll-behavior: contain;
		}
	}
</style>
