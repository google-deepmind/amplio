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
	// The criteria's placeholder where each tick of a loop is verified.
	export const TICK_CRITERIA = "Default: each tick's prompt, the request it re-sends";
</script>

<script lang="ts">
	// The verification form's fields: criteria, the verifier's model and the
	// round limit, edited in place. VerifyForm adds its actions; LoopForm embeds
	// them for "Verify each tick".
	import { serverSettings } from '$lib/serverSettings.svelte';
	import type { VerifyFields } from '$lib/composerOptions';
	import ModelSelect from './ModelSelect.svelte';

	let {
		value = $bindable(),
		criteriaPlaceholder = "Default: the agent's current request (its task, or the latest message that re-engaged it)",
		invalid = null,
		errId = ''
	}: {
		value: VerifyFields;
		criteriaPlaceholder?: string;
		invalid?: string | null; // the host's validation, shown on the rounds field
		errId?: string; // the id of the host's error line
	} = $props();

	const defaultModel = $derived(serverSettings.verify.model || "the agent's own model");

	// A number input reads as null when cleared, so the server's default applies.
	function setRounds(v: string) {
		value.max_rounds = v.trim() === '' ? null : Number(v);
	}
</script>

<label class="field">
	<span class="lbl">Criteria</span>
	<textarea rows="2" bind:value={value.criteria} placeholder={criteriaPlaceholder}></textarea>
</label>
<div class="row">
	<!-- Not a <label>: the picker is a button and a menu of buttons. -->
	<div class="field grow">
		<span class="lbl" aria-hidden="true">Verifier model</span>
		<ModelSelect bind:value={value.model} name="Verifier model" defaultLabel={defaultModel} placement="auto" />
	</div>
	<label class="field rounds">
		<span class="lbl">Max rounds</span>
		<input
			type="number"
			min="0"
			step="1"
			inputmode="numeric"
			value={value.max_rounds ?? ''}
			aria-invalid={!!invalid}
			aria-describedby={invalid && errId ? errId : undefined}
			oninput={(e) => setRounds(e.currentTarget.value)}
			placeholder={String(serverSettings.verify.max_rounds)}
		/>
	</label>
</div>

<style>
	.row {
		display: flex;
		gap: 0.6rem;
		flex-wrap: wrap;
	}
	.field {
		display: flex;
		flex-direction: column;
		gap: 0.2rem;
		min-width: 0;
	}
	.grow {
		flex: 1 1 14rem;
	}
	.rounds {
		flex: 0 0 7rem;
	}
	.lbl {
		font-size: var(--fs-sm);
		color: var(--text-dim);
	}
	@media (max-width: 640px) {
		.rounds {
			flex: 1 1 100%;
		}
	}
</style>
