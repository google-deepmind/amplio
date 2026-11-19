/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Loads a runes module (*.svelte.ts) in node, so `npm test` can exercise its
// classes: TypeScript's transpiler strips the types, Svelte compiles the runes,
// and the imports are pointed at files node loads ($lib/x is ../x.ts, other
// bare specifiers resolve from the frontend's node_modules).

import { readFileSync } from 'node:fs';
import ts from 'typescript';
import { compileModule } from 'svelte/compiler';

function target(spec: string, from: URL): string {
	if (spec.startsWith('$lib/')) {
		const path = spec.slice('$lib/'.length);
		if (path.endsWith('.svelte')) throw new Error(`${spec}: only plain TypeScript modules can be imported here`);
		return new URL(`../${path}${/\.[jt]s$/.test(path) ? '' : '.ts'}`, import.meta.url).href;
	}
	if (spec.startsWith('.')) return new URL(spec, from).href;
	return import.meta.resolve(spec);
}

export async function importRunes<T>(file: URL): Promise<T> {
	const source = readFileSync(file, 'utf8');
	const { outputText } = ts.transpileModule(source, {
		compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022, useDefineForClassFields: true }
	});
	const { code } = compileModule(outputText, { filename: file.pathname, generate: 'client' }).js;
	const linked = code.replace(/(\bfrom\s*|\bimport\s*)(['"])([^'"]+)\2/g, (_, kw, q, spec) => `${kw}${q}${target(spec, file)}${q}`);
	return import(`data:text/javascript,${encodeURIComponent(linked)}`);
}
