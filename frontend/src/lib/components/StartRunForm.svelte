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
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import { goto } from '$app/navigation';
	import { peekRunPrefill, takeRunPrefill } from '$lib/runPrefill.svelte';
	import { viewport } from '$lib/viewport.svelte';
	import { closeOnBack } from '$lib/pickerSheet.svelte';
	import ModelSelect from './ModelSelect.svelte';
	import BriefingSelect from './BriefingSelect.svelte';
	import WorkspaceField from './WorkspaceField.svelte';
	import {
		CommandIcon,
		KeyReturnIcon,
		CpuIcon,
		ChatCircleTextIcon,
		CircleNotchIcon
	} from 'phosphor-svelte';

	let mode = $state<'autonomous' | 'interactive'>('autonomous');
	let text = $state(''); // task (autonomous) or opening message (interactive)
	let title = $state('');
	let llm = $state('');
	let workspace = $state('.');
	let briefings = $state<string[]>([]);
	// Bound from WorkspaceField — the short label ("new · jj", "my-alias",
	// basename of path). Used on the Start button during the resolving stage
	// so the user sees WHICH workspace is being created without bloating the
	// button to "Creating workspace…" (which wrapped to a second line).
	let workspaceSummary = $state('');
	// Submit goes through two HTTP calls so the UI can show progress per
	// stage: 'resolving' creates the workspace (slow, ~5-30s when the backend
	// has to materialize one),
	// 'starting' fires the actual run (fast). Idle when nothing in flight.
	let stage = $state<'idle' | 'resolving' | 'starting'>('idle');
	const busy = $derived(stage !== 'idle');
	let error = $state('');
	let focused = $state(false);
	// Set once the operator edits any field; keeps the composer expanded on blur
	// so a half-written run isn't collapsed away. Cleared on submit/reset.
	let dirty = $state(false);
	let taEl = $state<HTMLTextAreaElement>();
	let formEl = $state<HTMLFormElement>();
	let isMac = $state(false);
	onMount(() => {
		isMac = /Mac|iP(hone|ad|od)/.test(navigator.userAgent);
	});

	// "New run like this": consume a prefill handed over by a dashboard run's
	// overflow menu. An $effect (not just onMount) so it fires even when the
	// composer is ALREADY mounted on the same page — the common case, since the
	// item is dashboard-only. peek to react, take to consume exactly once; dirty +
	// focus keep the composer expanded so the pre-filled run is visible and
	// editable rather than collapsed to the resting bar. Workspace is intentionally
	// not prefilled (see runPrefill): a variation gets a fresh workspace.
	$effect(() => {
		if (!peekRunPrefill()) return; // track; nothing pending
		const p = takeRunPrefill();
		if (!p) return;
		mode = p.interactive ? 'interactive' : 'autonomous';
		text = p.task;
		title = p.title;
		llm = p.llm;
		dirty = true;
		taEl?.focus();
	});

	const interactive = $derived(mode === 'interactive');
	// Resting state is a single-line bar; expands while focused or once edited
	// (dirty), so blurring a partially-filled form doesn't fold it shut.
	// Phone: the expanded form is a bottom sheet; tapping its scrim (or Escape)
	// folds it back to the resting bar and keeps the draft.
	let folded = $state(false);
	const expanded = $derived(!folded && (focused || dirty || text.trim().length > 0));

	// Back folds the sheet (a picker open inside it closes first).
	closeOnBack(
		() => expanded,
		() => fold()
	);

	function onFormClick(e: MouseEvent) {
		if (!viewport.isPhone || e.target !== formEl) return;
		if (e.clientY < formEl.getBoundingClientRect().top) fold();
	}
	function onFormKey(e: KeyboardEvent) {
		if (e.key === 'Escape' && viewport.isPhone && !e.defaultPrevented) fold();
	}
	function fold() {
		folded = true;
		focused = false;
		(document.activeElement as HTMLElement | null)?.blur();
	}

	function onFocusOut(e: FocusEvent) {
		const form = e.currentTarget as HTMLElement;
		const next = e.relatedTarget as Node | null;
		if (!next || !form.contains(next)) focused = false;
	}

	function setMode(m: 'autonomous' | 'interactive') {
		mode = m;
		taEl?.focus(); // keep the cursor where you were typing
	}

	// One submit key for both modes: Cmd/Ctrl+Enter. Plain Enter is always a
	// newline, so multi-line tasks/messages aren't fired off half-written.
	function onTaskKey(e: KeyboardEvent) {
		if (e.key !== 'Enter' || e.isComposing) return;
		if (e.metaKey || e.ctrlKey) {
			e.preventDefault();
			formEl?.requestSubmit();
		}
	}

	async function submit(e: Event) {
		e.preventDefault();
		if (!text.trim()) return;
		error = '';
		try {
			// Stage 1 (slow path only): pre-create the workspace when the spec
			// is `new:` / `anon:`. Materializing a workspace takes
			// 5-30s, so we want a visible "creating" stage. For other specs
			// (an existing workspace, a path) the resolve is a fast open — skip the
			// pre-create call entirely and let startRun's internal Resolve
			// handle it in one shot. No fake "creating" flash for sub-second
			// work.
			const wsSpec = workspace.trim() || '.';
			let wsForStart = wsSpec;
			if (wsSpec.startsWith('new:') || wsSpec.startsWith('anon:')) {
				stage = 'resolving';
				const { path } = await api.createWorkspace(wsSpec);
				wsForStart = path;
			}

			// Stage 2: start the run. For pre-created workspaces wsForStart
			// is an absolute path (fast open inside startRun); for other
			// specs it's the original spec (resolved server-side as before).
			// Title is optional in both modes; the server auto-titles from
			// the task / first message when left blank.
			stage = 'starting';
			const common = {
				title: title.trim() || undefined,
				llm: llm || undefined,
				workspace: wsForStart,
				briefings: briefings.length > 0 ? briefings : undefined
			};
			if (interactive) {
				const { run_id } = await api.startRun({ interactive: true, message: text, ...common });
				resetAndGo(`/runs/${run_id}/chat`); // follow up in the conversation
			} else {
				const { run_id } = await api.startRun({ task: text, ...common });
				resetAndGo(`/runs/${run_id}`); // watch it on the overview
			}
		} catch (err) {
			error = String(err);
		} finally {
			stage = 'idle';
		}
	}

	function resetAndGo(path: string) {
		text = '';
		title = '';
		focused = false;
		dirty = false;
		goto(path);
	}
</script>

<!-- The click/keydown handlers only fold the phone sheet (scrim tap, Escape);
     every control inside is a real input or button. -->
<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<form
	class="card composer"
	class:expanded
	class:interactive
	bind:this={formEl}
	onsubmit={submit}
	oninput={() => (dirty = true)}
	onfocusin={() => {
		focused = true;
		folded = false;
	}}
	onfocusout={onFocusOut}
	onclick={onFormClick}
	onkeydown={onFormKey}
>
	{#if expanded}
		<div class="head">
			<span class="mode-title">
				<!-- Same icons as the mode switcher below so the heading reads as
				     a labelled badge of the current run mode (visually echoes the
				     selected pill). Color is inherited via the .interactive
				     ancestor rule so it tints with the run mode. -->
				{#if interactive}
					<ChatCircleTextIcon size={16} weight="bold" />
				{:else}
					<CpuIcon size={16} weight="bold" />
				{/if}
				{interactive ? 'Interactively Driven Run' : 'Autonomous Run'}
			</span>
		</div>
		<input
			class="title"
			bind:value={title}
			placeholder="Title — optional (auto)"
			onkeydown={(e) => {
				if (e.key === 'Enter') {
					e.preventDefault();
					taEl?.focus();
				}
			}}
		/>
	{/if}
	<textarea
		class="task"
		class:collapsed={!expanded}
		bind:this={taEl}
		bind:value={text}
		rows={expanded ? 4 : 1}
		placeholder={interactive ? 'Message the agent to start a chat…' : 'Start a run…'}
		onkeydown={onTaskKey}
	></textarea>
	{#if expanded}
		<div class="footer">
			<div class="modes" role="group" aria-label="Run mode">
				<button
					type="button"
					class:on={!interactive}
					title="Autonomous — the agent runs the task to completion on its own"
					aria-label="Autonomous run"
					onmousedown={(e) => e.preventDefault()}
					onclick={() => setMode('autonomous')}><CpuIcon size={16} /></button
				>
				<button
					type="button"
					class:on={interactive}
					title="Interactive — drive the agent in a chat"
					aria-label="Interactive run"
					onmousedown={(e) => e.preventDefault()}
					onclick={() => setMode('interactive')}><ChatCircleTextIcon size={16} /></button
				>
			</div>
			<WorkspaceField
				bind:value={workspace}
				bind:summary={workspaceSummary}
				{interactive}
				ondirty={() => (dirty = true)}
			/>
			<ModelSelect bind:value={llm} onpick={() => taEl?.focus()} />
			<BriefingSelect bind:selected={briefings} ondirty={() => (dirty = true)} />
			<!-- Button label varies by stage. Idle: action verb + keycap hint.
			     Resolving: just the workspace summary (e.g. "new · jj") —
			     the spinner conveys "in progress" so the text doesn't need
			     a verb, and keeping it short prevents the button from
			     wrapping to a second line. Starting: brief action word
			     before navigation. -->
			<button class="primary start" class:interactive disabled={busy || !text.trim()}>
				<span>
					{#if stage === 'resolving'}
						{workspaceSummary || 'workspace'}
					{:else if stage === 'starting'}
						{interactive ? 'Sending…' : 'Starting…'}
					{:else}
						{interactive ? 'Send' : 'Start'}
					{/if}
				</span>
				{#if busy}
					<span class="spin"><CircleNotchIcon size={14} weight="bold" /></span>
				{:else}
					<kbd class="kbd">
						{#if isMac}<CommandIcon size={13} weight="bold" />{:else}Ctrl{/if}
						<KeyReturnIcon size={13} weight="bold" />
					</kbd>
				{/if}
			</button>
		</div>
	{/if}
	{#if error}<p class="err">{error}</p>{/if}
</form>

<style>
	/* One integrated input surface: title + textarea + controls share the box,
	   and the box border is the focus ring (color only, so nothing shifts). */
	.composer {
		padding: 0.6rem 0.8rem;
		transition: border-color 0.12s ease;
	}
	/* Resting bar: the textarea fills the whole box (padding included), so a click
	   anywhere expands it and the single line stays vertically centered. */
	.composer:not(.expanded) {
		padding: 0;
		cursor: text;
	}
	.composer:not(.expanded) .task {
		padding: 0.7rem 0.85rem;
	}
	.composer.expanded:focus-within {
		border-color: var(--accent);
	}
	.composer.interactive.expanded:focus-within {
		border-color: var(--chat);
	}
	/* Header: the mode toggle (icons) + a descriptive title naming the mode, so
	   the icons are self-explanatory. */
	.head {
		display: flex;
		align-items: center;
		gap: 0.6rem;
		margin-bottom: 0.4rem;
	}
	.mode-title {
		display: inline-flex;
		align-items: center;
		gap: 0.4rem;
		font-size: var(--fs-md);
		font-weight: 500;
		color: var(--accent);
	}
	.interactive .mode-title {
		color: var(--chat);
	}
	.title {
		background: transparent;
		border: none;
		font-size: var(--fs-lg);
		font-weight: 500;
		padding: 0.2rem 0.3rem;
		margin-bottom: 0.2rem;
	}
	.title::placeholder {
		color: var(--text-dim);
		font-weight: 400;
	}
	.task {
		background: transparent;
		border: none;
		padding: 0.2rem 0.3rem;
	}
	.task:focus {
		outline: none;
	}
	.task.collapsed {
		resize: none;
		overflow: hidden;
	}
	.expanded .task {
		min-height: 4.5rem;
		margin-bottom: 0.4rem;
	}
	.footer {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		flex-wrap: wrap;
	}
	.modes {
		display: inline-flex;
		gap: 0.15rem;
		background: var(--bg);
		border: 1px solid var(--border);
		border-radius: var(--radius-pill);
		padding: 0.12rem;
	}
	.modes button {
		display: inline-flex;
		align-items: center;
		background: none;
		border: none;
		border-radius: var(--radius-pill);
		padding: 0.3rem 0.55rem;
		color: var(--text-dim);
		cursor: pointer;
	}
	.modes button.on {
		background: var(--bg-elev2);
		color: var(--text);
	}
	.interactive .modes button.on {
		color: var(--chat);
	}
	.start {
		margin-left: auto;
		display: inline-flex;
		align-items: center;
		justify-content: center;
		gap: 0.45rem;
	}
	/* Submit spinner: occupies the trailing keycap's slot, so the button keeps
	   its idle width (no long text, no layout shift). Uses app.css's shared
	   @keyframes spin; just adds inline-flex layout + a faster spin. */
	.spin {
		display: inline-flex;
		animation: spin 0.8s linear infinite;
	}
	.start.interactive {
		background: var(--chat);
		border-color: var(--chat);
		color: var(--on-accent);
	}
	/* Keycap reads off the button's own text color, so it adapts to both the
	   accent (autonomous) and teal (chat) button backgrounds. */
	.kbd {
		display: inline-flex;
		align-items: center;
		gap: 0.1rem;
		font-size: var(--fs-xs);
		line-height: 1;
		padding: 0.14rem 0.3rem;
		border-radius: var(--radius-xs);
		background: color-mix(in srgb, currentColor 18%, transparent);
	}
	.err {
		font-size: var(--fs-md);
		margin: 0.5rem 0 0;
	}

	/* Phone: expanded, the form is a bottom sheet over a scrim. Its footer (the
	   pickers and Start) is a sticky action row, so with the keyboard up the
	   fields scroll under it and Start stays reachable. --app-h is the visual
	   viewport while a keyboard is open (see app.css); the bottom offset lifts
	   the sheet above an iOS keyboard, where 100dvh does not shrink. */
	@media (max-width: 640px) {
		.composer.expanded {
			position: fixed;
			z-index: 30;
			inset: auto 0 calc(100dvh - var(--app-h, 100dvh)) 0;
			max-height: calc(var(--app-h, 100dvh) - var(--bar-h));
			overflow-y: auto;
			overscroll-behavior: contain;
			padding: var(--sp-4) var(--gutter) 0;
			border-width: 1px 0 0;
			border-radius: var(--sheet-radius) var(--sheet-radius) 0 0;
			box-shadow: 0 0 0 100vmax var(--scrim);
		}
		/* Transparent tap catcher over the scrim: a tap there lands on the form
		   (see onFormClick) instead of the run list behind it. */
		.composer.expanded::before {
			content: '';
			position: fixed;
			inset: 0;
			z-index: -1;
		}
		.head {
			margin-bottom: var(--sp-2);
		}
		.title {
			padding: 0 var(--sp-1);
			margin-bottom: var(--sp-1);
		}
		.expanded .task {
			min-height: 0;
			margin-bottom: var(--sp-2);
			resize: none;
		}
		.footer {
			position: sticky;
			bottom: 0;
			gap: var(--sp-2);
			margin-inline: calc(-1 * var(--gutter));
			padding: var(--sp-2) var(--gutter) var(--sp-3);
			background: var(--bg-elev);
			border-top: 1px solid var(--border);
		}
		/* Two pickers per row; Start gets its own full-width row. */
		.footer > :global(.wrap) {
			flex: 1 1 9rem;
			min-width: 0;
		}
		.footer > :global(.wrap > .trigger) {
			width: 100%;
		}
		.modes button {
			position: relative;
			min-height: 36px;
			min-width: var(--tap);
			justify-content: center;
		}
		.modes button::after {
			content: '';
			position: absolute;
			inset: -4px 0;
		}
		.start {
			flex-basis: 100%;
			margin-left: 0;
		}
		.kbd {
			display: none;
		}
		/* The pickers' menus are bottom sheets too, over the form. The trigger's
		   ::after catches taps on their scrim, so a tap there closes the menu
		   (the trigger toggles it) instead of reaching the control underneath. */
		.footer :global(.wrap > .menu) {
			position: fixed;
			z-index: 32;
			inset: auto 0 calc(100dvh - var(--app-h, 100dvh)) 0;
			min-width: 0;
			max-width: none;
			max-height: calc(var(--app-h, 100dvh) * 0.7);
			overflow-y: auto;
			overscroll-behavior: contain;
			padding: 0 var(--sheet-pad-x, var(--sp-2)) var(--sp-3);
			background: var(--bg-elev);
			border: 1px solid var(--border);
			border-bottom: none;
			border-radius: var(--sheet-radius) var(--sheet-radius) 0 0;
			box-shadow: 0 0 0 100vmax var(--scrim);
		}
		/* Sheet header (in each picker's markup): handle, title and a Done that
		   stays in view while the sheet scrolls. */
		.footer :global(.wrap > .menu > .sheet-head) {
			position: sticky;
			top: 0;
			z-index: 1;
			display: flex;
			align-items: center;
			justify-content: space-between;
			margin-inline: calc(-1 * var(--sheet-pad-x, var(--sp-2)));
			padding: var(--sp-4) calc(var(--gutter) - var(--sp-3)) var(--sp-2) var(--gutter);
			background: var(--bg-elev);
			font-size: var(--fs-md);
			font-weight: 600;
		}
		.footer :global(.wrap > .menu > .sheet-head::before) {
			content: '';
			position: absolute;
			top: var(--sp-2);
			left: calc(50% - 16px);
			width: 32px;
			height: 4px;
			border-radius: 2px;
			background: var(--border);
		}
		.footer :global(.wrap > .menu > .sheet-head > .done) {
			padding: 0 var(--sp-3);
			background: none;
			border: none;
			color: var(--accent);
			font-weight: 600;
		}
		.footer :global(.wrap:has(> .menu) > .trigger::after) {
			content: '';
			position: fixed;
			inset: 0;
			z-index: 31;
		}
	}
</style>
