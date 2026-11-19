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
	// The inspector's Verification section for one session: its state, request
	// and history (GET .../verify), and, with the token, the verification form
	// to turn it on, change it or clear it, without sending a message.
	import { SealCheckIcon } from 'phosphor-svelte';
	import { api, errorText } from '$lib/api';
	import { auth } from '$lib/auth.svelte';
	import { formatTimestamp } from '$lib/time';
	import { verifyRequest, roundsText, type VerifyFields } from '$lib/composerOptions';
	import { changeDetail, changeText, labelOf, verifyActive, verifyState } from '$lib/verifyText';
	import type { SessionDTO, VerifyReport } from '$lib/types';
	import VerifyForm, { newVerifyFields } from './VerifyForm.svelte';
	import InspectSection from './InspectSection.svelte';

	let {
		runId,
		session,
		report,
		onreport
	}: {
		runId: string;
		session: SessionDTO;
		// The host loads it (and reloads it on the session's events); unset while
		// loading, and for a session that can't be verified (see why), whose
		// report the host doesn't ask for.
		report?: VerifyReport | null;
		// The report a change made here returned, for the host to keep: by
		// session id, as the reply can come after another session is shown.
		onreport: (sessionId: string, report: VerifyReport) => void;
	} = $props();

	let editing = $state(false);
	let busy = $state(false);
	let error = $state('');
	// A session that never concludes, or that is itself a verifier, can't be verified.
	const why = $derived(
		session.verifier_for
			? `${session.session_id} is ${session.verifier_for}'s verifier, and a verifier isn't verified.`
			: session.interactive
				? 'An interactive agent never concludes, so it is never verified.'
				: ''
	);
	const vs = $derived(report ? verifyState(report) : null);
	const active = $derived(verifyActive(report));
	const formValue = $derived<VerifyFields>(
		active && report
			? { criteria: report.criteria, model: report.model, max_rounds: report.max_rounds }
			: newVerifyFields()
	);

	async function save(f: VerifyFields) {
		const id = session.session_id;
		busy = true;
		error = '';
		try {
			onreport(id, await api.setVerify(runId, id, verifyRequest(f)));
			editing = false;
		} catch (e) {
			error = errorText(e);
		} finally {
			busy = false;
		}
	}
	async function clear() {
		const id = session.session_id;
		if (!confirm(`Turn off verification for ${id}? A conclusion its verifier is checking is withdrawn.`)) return;
		busy = true;
		error = '';
		try {
			onreport(id, await api.clearVerify(runId, id));
			editing = false;
		} catch (e) {
			error = errorText(e);
		} finally {
			busy = false;
		}
	}
</script>

{#snippet badge()}
	{#if vs}<span class="vstate vstate-{report?.state}">{vs.label}</span>{/if}
{/snippet}
{#snippet actions()}
	{#if auth.authed && !why && report && !editing}
		<button type="button" onclick={() => (editing = true)}>{active ? 'Edit' : 'Turn on'}</button>
		{#if active}<button type="button" class="danger" onclick={clear} disabled={busy}>Clear</button>{/if}
	{/if}
{/snippet}

<InspectSection title="Verification" icon={SealCheckIcon} {badge} {actions}>
	{#if why && !report}
		<p class="dim small">{why}</p>
	{:else if !report}
		<p class="dim small">Loading…</p>
	{:else}
		{#if why}<p class="dim small">{why}</p>{/if}
		{#if vs && !why}<p class="detail">{vs.detail}</p>{/if}
		{#if report.state !== 'off'}
			<dl class="kv">
				<dt>Criteria</dt>
				<dd class:dim={!report.criteria}>{report.criteria || "the agent's current request"}</dd>
				<dt>Verifier model</dt>
				<dd class:dim={!report.model}>{report.model || "the agent's own"}</dd>
				<dt>Max rounds</dt>
				<dd>{roundsText(report.max_rounds)}</dd>
				{#if report.version}
					<dt>Version</dt>
					<dd>{labelOf(report.version, report.candidate)}</dd>
				{/if}
				{#if report.verifier}
					<dt>Verifier</dt>
					<dd><a class="mono" href={`/runs/${runId}/sessions/${report.verifier}`}>{report.verifier}</a></dd>
				{/if}
			</dl>
		{/if}
		{#if editing}
			<div class="form">
				<VerifyForm
					value={formValue}
					submitLabel={active ? 'Replace' : 'Turn on'}
					autofocus
					notice={active
						? 'Saving starts a new version of the request: a conclusion the verifier is checking is withdrawn, and the same verifier checks the next one.'
						: session.status === 'concluded'
							? 'It applies from its next conclusion, after a message or a loop tick brings it back.'
							: ''}
					onsubmit={save}
					oncancel={() => {
						editing = false;
						error = '';
					}}
					{busy}
					{error}
				/>
			</div>
		{:else if error}
			<p class="err small">{error}</p>
		{/if}
		{#if report.history.length}
			<details class="history">
				<summary class="dim small">History ({report.history.length})</summary>
				<ol>
					{#each report.history as c, i (i)}
						<li>
							<span class="when dim small">{formatTimestamp(c.created_at)} · step {c.step}</span>
							<span>{changeText(c)}</span>
							{#if changeDetail(c)}<span class="req dim small">{changeDetail(c)}</span>{/if}
						</li>
					{/each}
				</ol>
			</details>
		{/if}
	{/if}
</InspectSection>

<style>
	.vstate {
		font-size: var(--fs-sm);
		padding: 0 0.5rem;
		border-radius: var(--radius-pill);
		border: 1px solid var(--border);
		color: var(--text-dim);
	}
	.vstate-on,
	.vstate-verifying {
		color: var(--accent);
		border-color: color-mix(in srgb, var(--accent) 45%, var(--border));
	}
	.vstate-passed {
		color: var(--ok);
		border-color: color-mix(in srgb, var(--ok) 45%, var(--border));
	}
	.vstate-not_verified {
		color: var(--warn);
		border-color: color-mix(in srgb, var(--warn) 45%, var(--border));
	}
	.detail {
		margin: 0;
	}
	.kv {
		display: grid;
		grid-template-columns: auto 1fr;
		gap: 0.2rem 1rem;
		margin: 0;
		font-size: var(--fs-md);
	}
	.kv dt {
		color: var(--text-dim);
	}
	.kv dd {
		margin: 0;
		overflow-wrap: anywhere;
		white-space: pre-wrap;
	}
	.form {
		border-top: 1px solid var(--border);
		padding-top: 0.6rem;
	}
	.history ol {
		margin: 0.4rem 0 0;
		padding-left: 1.2rem;
		display: flex;
		flex-direction: column;
		gap: 0.3rem;
	}
	.history li {
		display: flex;
		flex-wrap: wrap;
		gap: 0 0.6rem;
	}
	.req {
		flex-basis: 100%;
		overflow-wrap: anywhere;
	}
	.err {
		margin: 0;
	}
</style>
