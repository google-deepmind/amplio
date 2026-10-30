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
 PrivateNotice stands in for a page the readonly (shared) view can't show: the
 run list and recall need the owner's login, so a viewer sent one run's link
 sees this instead of an error.
-->
<script lang="ts">
	import { LockSimpleIcon } from 'phosphor-svelte';

	let { message }: { message: string } = $props();
</script>

<div class="private card" role="status">
	<LockSimpleIcon size={18} weight="bold" />
	<p>{message}</p>
</div>

<style>
	.private {
		display: flex;
		align-items: center;
		gap: 0.6rem;
		max-width: 40rem;
		margin: 1rem 0;
		color: var(--text-dim);
	}
	.private > :global(svg) {
		flex-shrink: 0;
	}
	p {
		margin: 0;
	}
</style>
