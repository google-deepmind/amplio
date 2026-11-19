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
	// A verifier's verdict as a badge: PASS, FAIL, or how it stopped without one,
	// with the round it was told in, or that it was on an earlier conclusion, or
	// stale (stale: on a candidate a newer version or a clear withdrew).
	import type { VerifyOutcome } from '$lib/types';
	import { verdictContext, verdictLabel, verdictStopped } from '$lib/verifyText';
	import { SealCheckIcon, WarningCircleIcon, XCircleIcon } from 'phosphor-svelte';

	let { outcome, stale = false }: { outcome: VerifyOutcome; stale?: boolean } = $props();
	const kind = $derived(verdictStopped(outcome) ? 'stopped' : outcome.verdict === 'PASS' ? 'pass' : 'fail');
	const round = $derived(verdictContext(outcome, stale));
</script>

<span class="verdict-badge {kind}" class:earlier={outcome.earlier}>
	{#if kind === 'pass'}<SealCheckIcon size={13} weight="fill" />
	{:else if kind === 'fail'}<XCircleIcon size={13} weight="fill" />
	{:else}<WarningCircleIcon size={13} weight="fill" />{/if}
	{verdictLabel(outcome)}
</span>
{#if round}<span class="dim small">{round}</span>{/if}

<style>
	.verdict-badge {
		display: inline-flex;
		align-items: center;
		gap: 0.2rem;
		padding: 0.02rem 0.35rem;
		border-radius: var(--radius-sm);
		font-size: var(--fs-xs);
		letter-spacing: 0.03em;
	}
	.pass {
		color: var(--ok);
		background: color-mix(in srgb, var(--ok) 15%, transparent);
	}
	.fail {
		color: var(--warn);
		background: color-mix(in srgb, var(--warn) 15%, transparent);
	}
	.stopped {
		color: var(--err);
		background: color-mix(in srgb, var(--err) 15%, transparent);
	}
	/* Information, not a judgement of the pending conclusion: muted. */
	.earlier {
		opacity: 0.7;
	}
</style>
