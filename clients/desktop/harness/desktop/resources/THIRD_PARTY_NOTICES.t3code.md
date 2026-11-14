# T3code-derived Codex integration portions

Morde Desktop contains dependency-free protocol and Codex item projection logic adapted from these T3code sources:

- `project/T3code/packages/effect-codex-app-server/src/protocol.ts` and `client.ts`: JSON-RPC frame validation/routing, bounded line handling, request correlation, and initialization sequencing.
- `project/T3code/apps/server/src/orchestration-v2/Adapters/CodexAdapterV2.ts`: visible Codex item to normalized event mappings for messages, reasoning summaries, plans, commands/tools, file changes, child agents, and approval-related items.

The Effect runtime was not copied or added as a Morde dependency. Morde uses its own Node.js child-process transport and event contract.

## MIT License

Copyright (c) 2026 T3 Tools Inc.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
