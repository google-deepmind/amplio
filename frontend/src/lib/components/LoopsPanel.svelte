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
	// The inspector's Loops section for one session: each active loop's summary,
	// prompt, whether each tick is verified, next tick and ticks so far, and,
	// with the token, the loop form to add, edit and stop them. Ended and
	// stopped loops are in the session's stream (the trajectory and chat show
	// them), not here.
	import { onMount } from 'svelte';
	import { RepeatIcon, SealCheckIcon } from 'phosphor-svelte';
	import { api, errorText, type LoopPatch } from '$lib/api';
	import { auth } from '$lib/auth.svelte';
	import { serverSettings } from '$lib/serverSettings.svelte';
	import {
		buildSpec,
		describeSpec,
		editFrom,
		fieldsForEdit,
		formatWhen,
		loopPatch,
		type EditOpened,
		type LoopFields
	} from '$lib/loopSchedule';
	import { loopVerifyFields, validateVerify, verifyPatch, verifyRequest, type VerifyFields } from '$lib/composerOptions';
	import { eachTickText } from '$lib/verifyText';
	import type { LoopDTO, SessionDTO } from '$lib/types';
	import LoopForm, { newLoopFields } from './LoopForm.svelte';
	import EachTickFields from './EachTickFields.svelte';
	import { newVerifyFields } from './VerifyForm.svelte';
	import InspectSection from './InspectSection.svelte';

	let {
		runId,
		session,
		loops,
		cancelled = '',
		onloops
	}: {
		runId: string;
		session: SessionDTO;
		loops?: LoopDTO[] | null; // the host loads them (and reloads them on the session's events); unset while loading
		cancelled?: string; // the session, or one above it, that is cancelled ("" if none)
		// The session's loops as reloaded after a change made here, for the host
		// to keep: by session id, as the reply can come after another is shown.
		onloops: (sessionId: string, loops: LoopDTO[]) => void;
	} = $props();

	// 'new', a loop id being edited, or null.
	let open = $state<string | null>(null);
	// The edit's starting point, taken when Edit is clicked: the list refreshes
	// on every tick, and a tick must not turn the operator's edit into another.
	let opened = $state<(EditOpened & { verify: VerifyFields | null }) | null>(null);
	let busy = $state(false);
	let error = $state('');
	const zone = $derived(serverSettings.displayZone);
	let now = $state(serverSettings.now());
	onMount(() => {
		const t = setInterval(() => (now = serverSettings.now()), 15_000);
		return () => clearInterval(t);
	});

	// Why no loop can be added (the server refuses both).
	const why = $derived(
		session.verifier_for
			? `${session.session_id} is a verifier; its ticks would revive it, so it can't have loops.`
			: cancelled
				? `${cancelled === session.session_id ? session.session_id : `${cancelled}, above it,`} is cancelled, and a loop's ticks must not revive it; send it a message first.`
				: ''
	);
	// Each tick can be verified only where the session concludes (the server
	// refuses it on an interactive agent), and by one of its loops at a time: a
	// session has one verification.
	const verifiable = $derived(!session.interactive);
	function verifyBlocked(except = ''): string {
		const other = (loops ?? []).find((l) => l.verify && l.id !== except);
		return other
			? `Loop ${other.id} verifies this session's ticks already; a session has one verification at a time, so only one of its loops can.`
			: '';
	}

	async function act(f: () => Promise<unknown>) {
		const id = session.session_id;
		busy = true;
		error = '';
		try {
			await f();
			open = null;
			onloops(id, await api.listLoops(runId, id));
		} catch (e) {
			error = errorText(e);
		} finally {
			busy = false;
		}
	}
	function add(f: LoopFields, prompt: string, verify: VerifyFields | null) {
		const spec = buildSpec(f, serverSettings.now());
		void act(() =>
			api.addLoop(runId, session.session_id, prompt.trim(), { kind: 'rrule', spec }, verify ? verifyRequest(verify) : undefined)
		);
	}
	function startEdit(l: LoopDTO) {
		opened = {
			fields: fieldsForEdit(l.schedule.spec, l.next_tick ? Date.parse(l.next_tick) : null),
			ticks: l.ticks,
			prompt: l.prompt,
			verify: loopVerifyFields(l.verify)
		};
		open = l.id;
		editPrompt = l.prompt;
		rawVerifyOn = !!l.verify;
		rawVerify = loopVerifyFields(l.verify) ?? newVerifyFields();
		error = '';
	}
	// verify: what the form shows, or undefined where it doesn't offer it.
	function edit(l: LoopDTO, after: LoopFields | null, prompt: string, verify?: VerifyFields | null) {
		if (!opened) return;
		let patch: LoopPatch;
		try {
			patch = loopPatch(l, opened, after, prompt, serverSettings.now());
		} catch (e) {
			error = errorText(e);
			return;
		}
		const v = verify === undefined ? undefined : verifyPatch(opened.verify, verify);
		if (v !== undefined) patch.verify = v;
		if (!patch.prompt && !patch.schedule && v === undefined) {
			open = null;
			return;
		}
		void act(() => api.editLoop(runId, session.session_id, l.id, patch));
	}
	function stop(l: LoopDTO) {
		if (!confirm(`Stop loop ${l.id} on ${session.session_id}?`)) return;
		void act(() => api.stopLoop(runId, session.session_id, l.id));
	}
	const relative = (iso: string) => {
		const m = Math.round((Date.parse(iso) - now) / 60_000);
		if (m <= 0) return 'due now';
		return m < 60 ? `in ${m}m` : m < 48 * 60 ? `in ${Math.round(m / 60)}h` : `in ${Math.round(m / 1440)}d`;
	};
	let editPrompt = $state('');
	// The editor for a rule the form doesn't write: its "Verify each tick".
	let rawVerifyOn = $state(false);
	let rawVerify = $state<VerifyFields>({ criteria: '', model: '', max_rounds: null });
</script>

{#snippet badge()}
	{#if loops?.length}<span class="count">{loops.length} active</span>{/if}
{/snippet}
{#snippet actions()}
	{#if auth.authed && !why && open !== 'new'}
		<button type="button" onclick={() => ((open = 'new'), (error = ''))}>Add a loop</button>
	{/if}
{/snippet}

<InspectSection title="Loops" icon={RepeatIcon} {badge} {actions}>
	{#if why}<p class="dim small">{why}</p>{/if}
	{#if open === 'new'}
		<div class="form">
			<LoopForm
				value={newLoopFields()}
				withPrompt
				withVerify={verifiable}
				verifyBlocked={verifyBlocked()}
				autofocus
				submitLabel="Add"
				onsubmit={add}
				oncancel={() => (open = null)}
				{busy}
				{error}
			/>
		</div>
	{/if}
	{#if !loops}
		<p class="dim small">Loading…</p>
	{:else if loops.length === 0 && open !== 'new'}
		<p class="dim small">No active loops. Stopped and ended loops stay in the session's trajectory.</p>
	{/if}
	{#each loops ?? [] as l (l.id)}
		{@const fields = fieldsForEdit(l.schedule.spec, l.next_tick ? Date.parse(l.next_tick) : null)}
		{@const left = fields?.ends.kind === 'times' ? fields.ends.n : null}
		<article class="loop">
			<div class="loop-head">
				<span class="lid mono dim">loop {l.id}</span>
				<span class="summary">{describeSpec(l.schedule.spec, l.summary, zone, now)}</span>
				{#if auth.authed && open !== l.id}
					<span class="loop-actions">
						<button type="button" onclick={() => startEdit(l)}>Edit</button>
						<button type="button" class="danger" onclick={() => stop(l)} disabled={busy}>Stop</button>
					</span>
				{/if}
			</div>
			{#if open === l.id && opened}
				<div class="form">
					{#if opened.ticks < l.ticks}
						<p class="hint small" role="status">
							Loop {l.id} ticked since you opened this; an "after N more ticks" end counts from then.
						</p>
					{/if}
					{#if opened.fields}
						<LoopForm
							value={opened.fields}
							prompt={opened.prompt}
							withPrompt
							withVerify={verifiable}
							verify={opened.verify}
							verifyBlocked={verifyBlocked(l.id)}
							editFrom={editFrom(l)}
							autofocus
							submitLabel="Save"
							onsubmit={(f, p, v) => edit(l, f, p, verifiable ? v : undefined)}
							oncancel={() => (open = null)}
							{busy}
							{error}
						/>
					{:else}
						<!-- A rule the form doesn't write (from the API or the CLI's
						     --rrule): its prompt can still change. -->
						<p class="dim small">
							This loop's rule isn't one the form writes; you can change its prompt{verifiable ? ' and whether each tick is verified' : ''}.
						</p>
						<textarea rows="2" bind:value={editPrompt}></textarea>
						{#if verifiable}
							<div class="raw-verify">
								<EachTickFields bind:on={rawVerifyOn} bind:value={rawVerify} blocked={verifyBlocked(l.id)} />
							</div>
						{/if}
						{#if error}<p class="err small">{error}</p>{/if}
						<div class="raw-actions">
							<button type="button" onclick={() => (open = null)}>Cancel</button>
							<button
								type="button"
								class="primary"
								disabled={busy || !editPrompt.trim() || (rawVerifyOn && !!validateVerify(rawVerify))}
								onclick={() =>
									edit(l, null, editPrompt, verifiable && !verifyBlocked(l.id) ? (rawVerifyOn ? $state.snapshot(rawVerify) : null) : undefined)}
								>Save</button
							>
						</div>
					{/if}
				</div>
			{:else}
				<p class="prompt">{l.prompt}</p>
				{#if l.verify}
					<p class="each-tick small"><SealCheckIcon size={13} weight="bold" />{eachTickText(l.verify)}</p>
				{/if}
			{/if}
			<p class="meta dim small">
				{#if l.next_tick}
					next tick {formatWhen(Date.parse(l.next_tick), zone, now, { zone: true })} ({relative(l.next_tick)})
				{:else}
					no tick left
				{/if}
				· {l.ticks} {l.ticks === 1 ? 'tick' : 'ticks'} so far
				{#if left !== null}· {left} left{/if}
				{#if l.last_tick}· last {formatWhen(Date.parse(l.last_tick), zone, now)}{/if}
			</p>
			{#if !fields}<p class="rule mono dim small">{l.schedule.spec}</p>{/if}
		</article>
	{/each}
	{#if error && open === null}<p class="err small">{error}</p>{/if}
</InspectSection>

<style>
	.count {
		font-size: var(--fs-sm);
		color: var(--accent);
	}
	.form {
		border-top: 1px solid var(--border);
		padding-top: 0.6rem;
	}
	.loop {
		border-top: 1px solid var(--border);
		padding-top: 0.5rem;
		display: flex;
		flex-direction: column;
		gap: 0.3rem;
	}
	.loop-head {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		flex-wrap: wrap;
	}
	.summary {
		color: var(--text-strong);
	}
	.loop-actions {
		margin-left: auto;
		display: inline-flex;
		gap: 0.4rem;
	}
	.prompt {
		margin: 0;
		white-space: pre-wrap;
		overflow-wrap: anywhere;
	}
	.each-tick {
		margin: 0;
		display: flex;
		align-items: center;
		gap: 0.3rem;
		color: var(--accent);
		overflow-wrap: anywhere;
	}
	.meta,
	.rule,
	.hint,
	.err {
		margin: 0;
	}
	.hint {
		color: var(--warn);
		margin-bottom: 0.4rem;
	}
	.rule {
		white-space: pre-wrap;
	}
	.raw-verify {
		margin-top: 0.4rem;
	}
	.raw-actions {
		display: flex;
		justify-content: flex-end;
		gap: 0.5rem;
		margin-top: 0.4rem;
	}
</style>
