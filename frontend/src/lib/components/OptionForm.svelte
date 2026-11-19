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
	// The form of a composer's open option (composer.open), under its title:
	// Verify, or Repeat. Applying it sets the option; then, or on Cancel, the
	// host closes it (ondone), wherever it shows it: inline, in a popover or in
	// a sheet. On a message, Verify with Repeat verifies each tick; on the
	// new-run form, the loop form has its own "Verify each tick".
	import { RepeatIcon, SealCheckIcon } from 'phosphor-svelte';
	import VerifyForm, { newVerifyFields } from './VerifyForm.svelte';
	import LoopForm, { newLoopFields } from './LoopForm.svelte';
	import type { ComposerState, Option } from '$lib/composer.svelte';

	let {
		composer,
		withPrompt = false,
		promptPlaceholder,
		withSendNow = true,
		loopTitle = 'Repeat this message',
		headId,
		ondone
	}: {
		composer: ComposerState;
		withPrompt?: boolean; // the loop has its own prompt (composer.loopPrompt), not the message
		promptPlaceholder?: string;
		withSendNow?: boolean;
		loopTitle?: string;
		headId?: string; // the title's id, for the dialog that shows the form
		ondone: (which: Option) => void; // applied or cancelled
	} = $props();

	const eachTick = $derived(!!composer.loop && !composer.ownLoopVerify);
</script>

{#if composer.open === 'verify'}
	<div class="head" id={headId}>
		<SealCheckIcon size={14} weight="bold" />{eachTick ? 'Verify each tick' : 'Verify the result'}
	</div>
	<VerifyForm
		value={composer.verify ?? newVerifyFields()}
		{eachTick}
		submitLabel={composer.verify ? 'Done' : 'Add'}
		autofocus={composer.focusForm}
		ondirty={(d) => (composer.dirty = d)}
		onsubmit={(f) => {
			composer.verify = f;
			ondone('verify');
		}}
		oncancel={() => ondone('verify')}
	/>
{:else if composer.open === 'loop'}
	<div class="head" id={headId}><RepeatIcon size={14} weight="bold" />{loopTitle}</div>
	<LoopForm
		value={composer.loop ?? { ...newLoopFields(), sendNow: withSendNow }}
		prompt={composer.loopPrompt}
		{withPrompt}
		{promptPlaceholder}
		promptOptional
		{withSendNow}
		withVerify={composer.ownLoopVerify && composer.offersVerify}
		verify={composer.loopVerify}
		submitLabel={composer.loop ? 'Done' : 'Add'}
		autofocus={composer.focusForm}
		ondirty={(d) => (composer.dirty = d)}
		onsubmit={(f, p, v) => {
			composer.loop = f;
			composer.loopPrompt = p;
			if (composer.ownLoopVerify) composer.loopVerify = v;
			ondone('loop');
		}}
		oncancel={() => ondone('loop')}
	/>
{/if}

<style>
	.head {
		display: flex;
		align-items: center;
		gap: 0.35rem;
		font-weight: 500;
		color: var(--text-strong);
		margin-bottom: 0.45rem;
	}
</style>
