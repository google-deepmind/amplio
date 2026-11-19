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

<script module lang="ts">
	export { newVerifyFields } from '$lib/composerOptions';
</script>

<script lang="ts">
	// The verification form: criteria, the verifier's model and
	// the round limit. The same component on the new-run form, the composer and
	// the inspector; the host decides what submitting does. A blank field is the
	// server's [verify] default, shown as its placeholder.
	import { onMount } from 'svelte';
	import { serverSettings } from '$lib/serverSettings.svelte';
	import { tickCostNote, validateVerify, type VerifyFields } from '$lib/composerOptions';
	import VerifyFieldset, { TICK_CRITERIA } from './VerifyFieldset.svelte';

	let {
		value,
		submitLabel,
		onsubmit,
		oncancel,
		onremove,
		removeLabel = 'Remove',
		eachTick = false,
		notice = '',
		error = '',
		busy = false,
		autofocus = false,
		ondirty
	}: {
		value: VerifyFields; // the starting values; the form edits its own copy
		submitLabel: string;
		onsubmit: (f: VerifyFields) => void;
		oncancel?: () => void;
		onremove?: () => void;
		removeLabel?: string;
		eachTick?: boolean; // it verifies each tick of a loop, not one conclusion
		notice?: string; // what submitting will do, where it isn't obvious
		error?: string; // the host's (e.g. the server refused the model)
		busy?: boolean;
		autofocus?: boolean; // take the focus when it opens
		ondirty?: (dirty: boolean) => void; // the edits differ from `value` (or no longer do)
	} = $props();

	// svelte-ignore state_referenced_locally
	let f = $state<VerifyFields>({ ...value });
	// svelte-ignore state_referenced_locally
	const initial = JSON.stringify(value);
	const dirty = $derived(JSON.stringify(f) !== initial);
	$effect(() => ondirty?.(dirty));
	const id = $props.id();
	let root = $state<HTMLElement>();
	onMount(() => {
		if (autofocus) root?.querySelector<HTMLElement>('textarea, input, select')?.focus();
	});
	const invalid = $derived(validateVerify(f));

	function submit() {
		if (!invalid && !busy) onsubmit({ ...f });
	}
	// Not a <form>: hosts (the composer, the new-run form) are forms already.
	// Enter in an input submits; Escape cancels (and only this form; not when
	// the model menu took it to close itself).
	function onKey(e: KeyboardEvent) {
		if (e.key === 'Enter' && (e.target as HTMLElement).tagName === 'INPUT' && !e.isComposing) {
			e.preventDefault();
			submit();
		} else if (e.key === 'Escape' && oncancel && !e.defaultPrevented) {
			e.preventDefault();
			e.stopPropagation();
			oncancel();
		}
	}
</script>

<!-- Enter in one of its inputs submits; the inputs are the interactive part. -->
<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<div class="vform" role="group" aria-label={eachTick ? 'Verify each tick' : 'Verification'} onkeydown={onKey} bind:this={root}>
	<VerifyFieldset bind:value={f} {invalid} errId="{id}-err" criteriaPlaceholder={eachTick ? TICK_CRITERIA : undefined} />
	<p class="hint dim small">
		An independent verifier checks the agent's conclusion before it's delivered; each rejection is a round
		(0 = unbounded). It costs a full agent run per check.
	</p>
	{#if eachTick}<p class="hint small cost">{tickCostNote(f.max_rounds ?? serverSettings.verify.max_rounds)}</p>{/if}
	{#if notice}<p class="hint small">{notice}</p>{/if}
	{#if invalid}<p class="err small" id="{id}-err" role="alert">{invalid}</p>{/if}
	{#if error}<p class="err small" role="alert">{error}</p>{/if}
	<div class="actions">
		{#if oncancel}<button type="button" onclick={oncancel}>Cancel</button>{/if}
		{#if onremove}<button type="button" class="danger" onclick={onremove} disabled={busy}>{removeLabel}</button>{/if}
		<button type="button" class="primary submit" disabled={!!invalid || busy} onclick={submit}>{submitLabel}</button>
	</div>
</div>

<style>
	.vform {
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
	}
	.hint,
	.err {
		margin: 0;
	}
	.cost {
		color: var(--warn);
	}
	.actions {
		display: flex;
		gap: 0.5rem;
		justify-content: flex-end;
		flex-wrap: wrap;
	}
	@media (max-width: 640px) {
		.actions button {
			flex: 1 1 0;
		}
	}
</style>
