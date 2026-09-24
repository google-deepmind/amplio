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

<!--
 Stop notice: the turn ended abnormally (cut off at the output limit, a
 malformed tool call, a provider-side filter, …), so what's shown above it may
 be incomplete. The server decides which stop reasons qualify; refusals have
 their own notice (Refusal.svelte). The message is the provider's own, plain
 text (never markdown).
-->
<script lang="ts">
	import type { StopNotice } from '$lib/types';
	import { WarningIcon } from 'phosphor-svelte';

	let { notice }: { notice: StopNotice } = $props();
</script>

<div class="stop" role="note">
	<div class="head">
		<WarningIcon size={14} weight="bold" />
		<span class="title">{notice.truncated ? 'Output cut off' : 'Stopped unexpectedly'}</span>
		<span class="reason">{notice.reason}</span>
	</div>
	{#if notice.message}
		<div class="why">{notice.message}</div>
	{/if}
</div>

<style>
	.stop {
		margin: 0.3rem 0;
		padding: 0.45rem 0.65rem;
		border: 1px solid color-mix(in srgb, var(--warn) 40%, transparent);
		border-left: 3px solid var(--warn);
		border-radius: var(--radius-sm);
		background: color-mix(in srgb, var(--warn) 8%, transparent);
		font-size: var(--fs-sm);
	}
	.head {
		display: flex;
		align-items: center;
		gap: 0.4rem;
		color: var(--warn);
	}
	.title {
		font-weight: 600;
	}
	.reason {
		font-family: var(--mono);
		font-size: 0.85em;
		padding: 0 0.35rem;
		border: 1px solid color-mix(in srgb, var(--warn) 45%, transparent);
		border-radius: var(--radius-sm);
	}
	.why {
		margin-top: 0.25rem;
		color: var(--text);
		white-space: pre-wrap;
		overflow-wrap: anywhere;
	}
</style>
