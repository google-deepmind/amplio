// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package lessons provides an in-memory similarity index over mined lessons,
// with confidence-weighted ranking. Unlike the skill index it reads from the DB:
// with an API embedder lessons carry their own stored embeddings, so Build does
// no embedding — only Search embeds (the query); the built-in local embedder
// and the BM25-only mode index every lesson's description instead (see Index).
// Ranking is by relevance plus a small, bounded adjustment derived from each
// lesson's accumulated score and load count: a well-rated lesson moves up among
// about-equally relevant ones, and a lesson judged unhelpful or harmful drops
// out of view, but relevance decides.
package lessons
