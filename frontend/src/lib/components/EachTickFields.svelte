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
	// "Verify each tick": a checkbox that reveals the verification form's fields
	// and what verifying every tick means. LoopForm embeds it, and so does the
	// Inspector's editor for a rule the loop form doesn't write.
	import { serverSettings } from '$lib/serverSettings.svelte';
	import { tickCostNote, validateVerify, type VerifyFields } from '$lib/composerOptions';
	import VerifyFieldset, { TICK_CRITERIA } from './VerifyFieldset.svelte';

	let {
		on = $bindable(),
		value = $bindable(),
		blocked = ''
	}: {
		on: boolean;
		value: VerifyFields;
		blocked?: string; // why it can't be turned on here (another loop verifies the session's ticks)
	} = $props();

	const id = $props.id();
	const errId = `${id}-err`;
	const invalid = $derived(on ? validateVerify(value) : null);
</script>

{#if blocked}
	<p class="blocked dim small">{blocked}</p>
{:else}
	<div class="verify">
		<label class="check">
			<input type="checkbox" bind:checked={on} />
			<span>Verify each tick <span class="dim">— a verifier checks the conclusion to every tick</span></span>
		</label>
		{#if on}
			<div class="vfields">
				<VerifyFieldset bind:value criteriaPlaceholder={TICK_CRITERIA} {invalid} {errId} />
				<p class="cost small">{tickCostNote(value.max_rounds ?? serverSettings.verify.max_rounds)}</p>
				{#if invalid}<p class="err small" id={errId} role="alert">{invalid}</p>{/if}
			</div>
		{/if}
	</div>
{/if}

<style>
	.verify {
		display: flex;
		flex-direction: column;
		gap: 0.4rem;
	}
	.check {
		display: flex;
		align-items: baseline;
		gap: 0.45rem;
	}
	.vfields {
		display: flex;
		flex-direction: column;
		gap: 0.45rem;
		padding-left: 1.4rem;
	}
	.cost {
		margin: 0;
		color: var(--warn);
	}
	.blocked,
	.err {
		margin: 0;
	}
</style>
