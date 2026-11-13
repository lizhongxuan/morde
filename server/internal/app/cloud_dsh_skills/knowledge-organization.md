---
author: Morde
name: knowledge-organization
description: Find and organize notes stored in the current authorized Workspace.
version: "1"
---

# Knowledge organization

- Search only the Workspace bound to the current authorized Task. Use `morde_workspace_list` and `morde_workspace_search` to locate files, then `morde_workspace_read` to inspect a candidate before answering or editing it. These tools do not search the user's other devices, local folders, or external accounts.
- When a request could refer to several files, show the likely matches and ask which one to use. If no matching file is found, say which authorized Workspace was searched; do not imply that other note services or locations were checked.
- Before revising a note, preserve its existing structure, identifiers, links, and unrelated content. Keep quoted source text distinct from summaries or new conclusions, and include source links only when present in the authorized material or supplied by the user.
- Make a new note only when requested or clearly needed for the request. For a large collection, inspect representative files and propose an index or structure before making broad changes.
- The available Workspace tools can list, read, search, and write files; they do not provide move, rename, or delete operations. Do not claim to reorganize files by moving or deleting them. If a new index would help, explain the proposal and wait for the user's direction when it changes the collection materially.
- Write only through `morde_workspace_write`, with the current expected revision and any required approval. Verify saved text with `morde_workspace_read` at the returned revision when available. If no authorized Workspace or required tool is available, explain the limitation and do not access an arbitrary path.
