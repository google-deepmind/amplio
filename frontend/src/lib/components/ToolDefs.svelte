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
	// The tools an agent of this session's type would be given. Shown in the
	// bootstrap phase, next to the system prompt, because the two together are
	// "what this agent was handed before it did anything".
	//
	// Fetched lazily on first expand: the trajectory viewer is opened constantly
	// and most visits are not about the toolset, so the request is only made when
	// someone asks to see it.
	import { api } from '$lib/api';
	import { errorText } from '$lib/api';
	import type { SessionTools, ToolDef } from '$lib/types';
	import { CaretRightIcon, WrenchIcon } from 'phosphor-svelte';

	let { runId, sessionId }: { runId: string; sessionId: string } = $props();

	let open = $state(false);
	let data = $state<SessionTools | null>(null);
	let error = $state('');
	let loading = $state(false);
	// Which tools have their raw schema showing. Structured params answer the
	// usual question ("what can I pass?"); the raw schema answers the exact one
	// ("what did the model actually receive?"), and only that one is authoritative.
	let raw = $state<Record<string, boolean>>({});

	// No $effect to reset on a session change: calling load() inside one makes its
	// $state reads AND writes dependencies of the effect, so `loading = true`
	// re-runs it, which wiped `data` the instant it arrived. The parent keys this
	// component by session id instead, so a different session gets a fresh
	// instance — cheaper to reason about than a correctly-untracked effect.

	async function load() {
		if (loading) return;
		loading = true;
		try {
			data = await api.getSessionTools(runId, sessionId);
			error = '';
		} catch (e) {
			error = errorText(e);
		} finally {
			loading = false;
		}
	}

	function toggle() {
		open = !open;
		if (open && !data && !loading) load();
	}

	type Param = { name: string; type: string; required: boolean; description: string };

	// JSON Schema is a big spec; this reads the handful of keys our tools emit
	// (object with properties, each with type/description, plus required[]).
	// Anything richer — oneOf, nested objects, enums — is where the raw view
	// earns its place, so this stays deliberately shallow rather than growing
	// into a half-correct schema renderer.
	function params(schema: unknown): Param[] {
		const s = schema as
			| { properties?: Record<string, { type?: string; description?: string }>; required?: string[] }
			| undefined;
		if (!s?.properties) return [];
		const required = new Set(s.required ?? []);
		return Object.entries(s.properties).map(([name, p]) => ({
			name,
			type: p?.type ?? '',
			required: required.has(name),
			description: p?.description ?? ''
		}));
	}

	function pretty(schema: unknown): string {
		try {
			return JSON.stringify(schema, null, 2);
		} catch {
			return String(schema);
		}
	}

	function toolKey(t: ToolDef, i: number): string {
		return t.name || `tool-${i}`;
	}
</script>

<section class="tools">
	<button class="head" onclick={toggle} aria-expanded={open}>
		<span class="caret" class:open><CaretRightIcon size={12} weight="bold" /></span>
		<WrenchIcon size={13} weight="bold" />
		<span class="label">Tools</span>
		{#if data}<span class="count dim">{data.tools.length}</span>{/if}
		{#if open && data?.cwd}<span class="cwd mono dim" title="Workspace root in the descriptions">{data.cwd}</span>{/if}
	</button>

	{#if open}
		{#if loading && !data}
			<p class="dim small pad">Loading…</p>
		{:else if error}
			<p class="err pad">{error}</p>
		{:else if data && !data.known}
			<p class="dim small pad">
				The <code>{data.agent_type}</code> agent type does not describe its tools.
			</p>
		{:else if data}
			<!-- Said plainly, because it is the one way this view can mislead: it is
			     built from today's code and this server's current corpora, not from a
			     record of what the session was given. -->
			<p class="note dim small pad">
				Reconstructed live for <code>{data.agent_type}</code> — what an agent of this type would
				receive now, not a record of what this session had.
			</p>
			{#each data.tools as t, i (toolKey(t, i))}
				{@const ps = params(t.schema)}
				<div class="tool">
					<div class="trow">
						<code class="tname">{t.name}</code>
						<button class="rawbtn" onclick={() => (raw[toolKey(t, i)] = !raw[toolKey(t, i)])}>
							{raw[toolKey(t, i)] ? 'hide schema' : 'schema'}
						</button>
					</div>
					<p class="tdesc">{t.description}</p>
					{#if ps.length}
						<ul class="params">
							{#each ps as p (p.name)}
								<li>
									<code class="pname">{p.name}</code>
									{#if p.type}<span class="ptype dim">{p.type}</span>{/if}
									{#if p.required}<span class="preq">required</span>{/if}
									{#if p.description}<span class="pdesc dim">{p.description}</span>{/if}
								</li>
							{/each}
						</ul>
					{:else}
						<p class="dim small noargs">no parameters</p>
					{/if}
					{#if raw[toolKey(t, i)]}
						<pre class="rawschema mono">{pretty(t.schema)}</pre>
					{/if}
				</div>
			{/each}
		{/if}
	{/if}
</section>

<style>
	.tools {
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		margin-bottom: 0.6rem;
		background: var(--bg-elev);
	}
	.head {
		display: flex;
		align-items: center;
		gap: 0.4rem;
		width: 100%;
		padding: 0.45rem 0.6rem;
		background: transparent;
		border: 0;
		color: var(--text);
		cursor: pointer;
		font-size: var(--fs-sm);
		text-align: left;
	}
	.head:hover {
		background: var(--bg-elev2);
	}
	.caret {
		display: inline-flex;
		transition: transform 120ms;
	}
	.caret.open {
		transform: rotate(90deg);
	}
	.label {
		font-weight: 500;
	}
	.cwd {
		margin-left: auto;
		font-size: var(--fs-xs);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		max-width: 40%;
	}
	.pad {
		padding: 0 0.7rem 0.5rem;
		margin: 0;
	}
	.note {
		border-bottom: 1px solid var(--border);
		padding-bottom: 0.5rem;
	}
	.tool {
		padding: 0.5rem 0.7rem;
		border-top: 1px solid var(--border);
	}
	.trow {
		display: flex;
		align-items: baseline;
		gap: 0.5rem;
	}
	.tname {
		font-size: var(--fs-sm);
		font-weight: 500;
		color: var(--accent);
	}
	.rawbtn {
		margin-left: auto;
		background: transparent;
		border: 0;
		padding: 0;
		color: var(--text-dim);
		cursor: pointer;
		font-size: var(--fs-xs);
	}
	.rawbtn:hover {
		color: var(--text);
		text-decoration: underline;
	}
	.tdesc {
		margin: 0.2rem 0 0.35rem;
		font-size: var(--fs-sm);
		white-space: pre-wrap;
	}
	.params {
		margin: 0;
		padding-left: 0.9rem;
		list-style: none;
	}
	.params li {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: 0.4rem;
		font-size: var(--fs-sm);
		padding: 0.1rem 0;
	}
	.pname {
		font-weight: 500;
	}
	.ptype,
	.pdesc {
		font-size: var(--fs-xs);
	}
	.pdesc {
		flex: 1 1 14rem;
		min-width: 0;
	}
	.preq {
		font-size: var(--fs-xs);
		color: var(--warn, #e0a030);
	}
	.noargs {
		margin: 0;
	}
	.rawschema {
		margin: 0.4rem 0 0;
		padding: 0.5rem;
		border-radius: var(--radius-sm);
		background: var(--bg);
		border: 1px solid var(--border);
		font-size: var(--fs-xs);
		overflow-x: auto;
		white-space: pre;
	}
	.err {
		color: var(--danger, #e06c6c);
	}
</style>
