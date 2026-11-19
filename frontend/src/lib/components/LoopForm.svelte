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
	import { serverSettings } from '$lib/serverSettings.svelte';
	import { defaultEnds, type LoopFields } from '$lib/loopSchedule';

	// A new loop as the form first shows it: no interval yet (it's required),
	// send now on, and the server's [loop] expiry as the end.
	export function newLoopFields(): LoopFields {
		return { every: null, unit: 'minutes', sendNow: true, ends: defaultEnds(serverSettings.loop.expiry) };
	}
</script>

<script lang="ts">
	// The loop form: interval, prompt, send now, ends, and (where the target
	// concludes) whether each tick is verified. The same component on the
	// new-run form, the composer and the inspector; the host builds the rule
	// (lib/loopSchedule) when it submits, so DTSTART is that moment, and decides
	// what submitting does.
	import { onMount } from 'svelte';
	import { validateVerify, type VerifyFields } from '$lib/composerOptions';
	import EachTickFields from './EachTickFields.svelte';
	import { newVerifyFields } from './VerifyForm.svelte';
	import {
		buildSpec,
		describeSpec,
		endsAt,
		firstTick,
		formatWhen,
		validate,
		type EditFrom,
		type Unit
	} from '$lib/loopSchedule';

	let {
		value,
		prompt: initialPrompt = '',
		withPrompt = false,
		promptPlaceholder = 'What to send each time',
		promptOptional = false,
		withSendNow = true,
		withVerify = false,
		verify = null,
		verifyBlocked = '',
		editFrom,
		submitLabel,
		onsubmit,
		oncancel,
		onremove,
		removeLabel = 'Remove',
		error = '',
		busy = false,
		autofocus = false,
		ondirty
	}: {
		value: LoopFields; // the starting values; the form edits its own copy
		prompt?: string;
		withPrompt?: boolean; // the composer's prompt is the message itself
		promptPlaceholder?: string;
		promptOptional?: boolean; // the new-run form falls back to the task
		withSendNow?: boolean;
		withVerify?: boolean; // offer "Verify each tick" (the target concludes)
		verify?: VerifyFields | null; // the verification each tick starts, to begin with
		verifyBlocked?: string; // why "Verify each tick" can't be turned on (another loop verifies)
		// Editing a loop: its schedule so far, which the rule starts from (see
		// firstTick); send now doesn't apply.
		editFrom?: EditFrom;
		submitLabel: string;
		// verify: what each tick verifies with, or null (always null without withVerify).
		onsubmit: (f: LoopFields, prompt: string, verify: VerifyFields | null) => void;
		oncancel?: () => void;
		onremove?: () => void;
		removeLabel?: string;
		error?: string;
		busy?: boolean;
		autofocus?: boolean; // take the focus when it opens
		ondirty?: (dirty: boolean) => void; // the edits differ from `value` (or no longer do)
	} = $props();

	// svelte-ignore state_referenced_locally
	let f = $state<LoopFields>(structuredClone($state.snapshot(value)));
	// svelte-ignore state_referenced_locally
	let prompt = $state(initialPrompt);
	// The end the loop had when the form opened, offered while editing.
	// svelte-ignore state_referenced_locally
	const keptEnd = value.ends.kind === 'until' ? value.ends.at : null;
	// The other end choices remember their numbers when switched away from; a
	// duration starts as [loop] expiry (7 days if that is "never").
	// svelte-ignore state_referenced_locally
	const startAfter =
		value.ends.kind === 'after' ? value.ends : (() => {
			const d = defaultEnds(serverSettings.loop.expiry);
			return d.kind === 'after' ? d : defaultEnds('');
		})();
	let afterN = $state(startAfter.kind === 'after' ? startAfter.n : 7);
	let afterUnit = $state<Unit>(startAfter.kind === 'after' ? startAfter.unit : 'days');
	// svelte-ignore state_referenced_locally
	let timesN = $state(value.ends.kind === 'times' ? value.ends.n : 5);
	// "Verify each tick": its fields are kept while it is switched off.
	// svelte-ignore state_referenced_locally
	let verifyOn = $state(withVerify && verify !== null);
	// svelte-ignore state_referenced_locally
	let vf = $state<VerifyFields>(verify ? { ...verify } : newVerifyFields());
	const verifyNow = $derived(withVerify && !verifyBlocked && verifyOn ? vf : null);

	const editing = $derived(editFrom !== undefined);
	const zone = $derived(serverSettings.displayZone);

	// svelte-ignore state_referenced_locally
	const initial = JSON.stringify([value, initialPrompt, withVerify ? verify : null]);
	const dirty = $derived(JSON.stringify([f, prompt, verifyNow]) !== initial);
	$effect(() => ondirty?.(dirty));
	const id = $props.id();
	const group = `${id}-ends`; // the Ends radios' name, unique per form
	let root = $state<HTMLElement>();

	// The end date and the summary are relative to now (the server's clock);
	// keep them current.
	let now = $state(serverSettings.now());
	onMount(() => {
		if (autofocus) root?.querySelector<HTMLElement>('input, textarea, select')?.focus();
		const t = setInterval(() => (now = serverSettings.now()), 15_000);
		return () => clearInterval(t);
	});

	function setEnds(kind: LoopFields['ends']['kind']) {
		if (kind === 'after') f.ends = { kind, n: afterN, unit: afterUnit };
		else if (kind === 'times') f.ends = { kind, n: timesN };
		else if (kind === 'until' && keptEnd !== null) f.ends = { kind, at: keptEnd };
		else f.ends = { kind: 'never' };
	}
	const num = (v: string) => (v.trim() === '' ? null : Number(v));

	// The loop's own fields; "Verify each tick" shows its own error.
	const loopInvalid = $derived(
		validate(f, serverSettings.loop.min_interval_seconds, now, editFrom) ??
			(withPrompt && !promptOptional && !prompt.trim() ? 'Write the prompt it sends.' : null)
	);
	const invalid = $derived(loopInvalid ?? (verifyNow ? validateVerify(verifyNow) : null));
	const end = $derived(endsAt(f, now));
	const summary = $derived.by(() => {
		if (loopInvalid) return '';
		const first = firstTick(f, now, editFrom);
		const tick = editing ? 'next tick' : 'first tick';
		const when = first <= now ? `${tick} now` : `${tick} ${formatWhen(first, zone, now, { zone: true })}`;
		return `${describeSpec(buildSpec(f, now, editFrom), '', zone, now)} · ${when}`;
	});

	function submit() {
		if (!invalid && !busy) onsubmit($state.snapshot(f) as LoopFields, prompt, verifyNow && { ...verifyNow });
	}
	// Enter in an input submits; Escape cancels (and only this form; not when
	// "Verify each tick"'s model menu took it to close itself).
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

{#snippet unitSelect(unit: Unit, set: (u: Unit) => void, label: string)}
	<select value={unit} onchange={(e) => set(e.currentTarget.value as Unit)} aria-label={label}>
		<option value="minutes">minutes</option>
		<option value="hours">hours</option>
		<option value="days">days</option>
	</select>
{/snippet}

<!-- Not a <form>: hosts (the composer, the new-run form) are forms already.
     Enter in one of its inputs submits. -->
<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<div class="lform" role="group" aria-label="Loop" onkeydown={onKey} bind:this={root}>
	<div class="line">
		<span class="lbl">Every</span>
		<input
			class="n"
			type="number"
			min="1"
			step="1"
			inputmode="numeric"
			placeholder="30"
			aria-label="Interval"
			aria-invalid={!!loopInvalid && f.every !== null}
			aria-describedby={loopInvalid && f.every !== null ? `${id}-err` : undefined}
			value={f.every ?? ''}
			oninput={(e) => (f.every = num(e.currentTarget.value))}
		/>
		{@render unitSelect(f.unit, (u) => (f.unit = u), 'Interval unit')}
	</div>
	{#if withSendNow && !editing}
		<label class="check">
			<input type="checkbox" bind:checked={f.sendNow} />
			<span>Send now <span class="dim">— the first message goes at once, then every interval</span></span>
		</label>
	{/if}
	{#if withPrompt}
		<label class="field">
			<span class="lbl">Prompt</span>
			<textarea rows="2" bind:value={prompt} placeholder={promptPlaceholder}></textarea>
		</label>
	{/if}
	<fieldset class="ends">
		<legend class="lbl">Ends</legend>
		<label class="choice">
			<input type="radio" name={group} checked={f.ends.kind === 'after'} onchange={() => setEnds('after')} />
			<span>after</span>
			<input
				class="n"
				type="number"
				min="1"
				step="1"
				inputmode="numeric"
				aria-label="Duration"
				value={afterN ?? ''}
				oninput={(e) => {
					afterN = num(e.currentTarget.value);
					setEnds('after');
				}}
			/>
			{@render unitSelect(
				afterUnit,
				(u) => {
					afterUnit = u;
					setEnds('after');
				},
				'Duration unit'
			)}
			{#if f.ends.kind === 'after' && end !== null}
				<span class="when dim small">{formatWhen(end, zone, now, { zone: true })}</span>
			{/if}
		</label>
		<label class="choice">
			<input type="radio" name={group} checked={f.ends.kind === 'times'} onchange={() => setEnds('times')} />
			<span>after</span>
			<input
				class="n"
				type="number"
				min="1"
				step="1"
				inputmode="numeric"
				aria-label="Ticks"
				value={timesN ?? ''}
				oninput={(e) => {
					timesN = num(e.currentTarget.value);
					setEnds('times');
				}}
			/>
			<span>{editing ? 'more ticks' : 'ticks'}</span>
		</label>
		{#if keptEnd !== null}
			<label class="choice">
				<input type="radio" name={group} checked={f.ends.kind === 'until'} onchange={() => setEnds('until')} />
				<span>at {formatWhen(keptEnd, zone, now, { zone: true })} <span class="dim">(unchanged)</span></span>
			</label>
		{/if}
		<label class="choice">
			<input type="radio" name={group} checked={f.ends.kind === 'never'} onchange={() => setEnds('never')} />
			<span>never</span>
		</label>
	</fieldset>
	{#if withVerify}<EachTickFields bind:on={verifyOn} bind:value={vf} blocked={verifyBlocked} />{/if}
	{#if summary}<p class="summary small">{summary}</p>{/if}
	{#if loopInvalid && f.every !== null}<p class="err small" id="{id}-err" role="alert">{loopInvalid}</p>{/if}
	{#if error}<p class="err small" role="alert">{error}</p>{/if}
	<div class="actions">
		{#if oncancel}<button type="button" onclick={oncancel}>Cancel</button>{/if}
		{#if onremove}<button type="button" class="danger" onclick={onremove} disabled={busy}>{removeLabel}</button>{/if}
		<button type="button" class="primary submit" disabled={!!invalid || busy} onclick={submit}>{submitLabel}</button>
	</div>
</div>

<style>
	.lform {
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
	}
	.line,
	.choice {
		display: flex;
		align-items: center;
		gap: 0.45rem;
		flex-wrap: wrap;
	}
	.lbl {
		font-size: var(--fs-sm);
		color: var(--text-dim);
	}
	.line .lbl {
		min-width: 2.6rem;
	}
	/* Beat app.css's full-width text fields: these sit inside a sentence. */
	.lform input.n {
		width: 5rem;
	}
	.lform select {
		width: auto;
	}
	.field {
		display: flex;
		flex-direction: column;
		gap: 0.2rem;
	}
	.check {
		display: flex;
		align-items: baseline;
		gap: 0.45rem;
	}
	.ends {
		border: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.3rem;
	}
	.ends legend {
		padding: 0;
		margin-bottom: 0.2rem;
	}
	.when {
		white-space: nowrap;
	}
	.summary {
		margin: 0;
		color: var(--accent);
	}
	.err {
		margin: 0;
	}
	.actions {
		display: flex;
		gap: 0.5rem;
		justify-content: flex-end;
		flex-wrap: wrap;
	}
	@media (max-width: 640px) {
		.lform input.n {
			width: 4.5rem;
		}
		.actions button {
			flex: 1 1 0;
		}
	}
</style>
