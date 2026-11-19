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
	// The Processes tab became the Inspector; old links land on its process list.
	import { page } from '$app/state';
	import { goto } from '$app/navigation';

	$effect(() => {
		const id = page.params.id;
		if (id) void goto(`/runs/${id}/inspector${page.url.search}#processes`, { replaceState: true });
	});
</script>

<p class="dim">Loading…</p>
