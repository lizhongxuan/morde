---
author: Morde
name: task-management
description: Organize a user-requested commitment list in the current authorized Workspace.
version: "1"
---

# Task management

- Keep the user's commitment list separate from the active Morde Task. This skill does not provide a general personal task manager or a connector to an external task app.
- Work only in the Workspace bound to the current authorized Task. Use `morde_workspace_list` or `morde_workspace_search` to find a named list; read it with `morde_workspace_read` before proposing or making changes. `morde_read_task_attachment` can read only attachments exposed for this Task, when that tool is available.
- If the user asks to update a list that is missing or ambiguous, ask which Workspace file they mean. Create a new Workspace checklist only when the user explicitly asks for one and its location is clear.
- Search for an existing item before adding it. Use a short, concrete next action; preserve existing identifiers, links, and unrelated content. Include only dates, owners, priority, and source details supported by the user's request or the authorized files. Do not invent reminders, recurrence, assignments, or completion.
- Before changing a list, describe the intended change when it affects many items. Write only through `morde_workspace_write`, using the current expected revision and any required approval. A successful write creates a new Workspace revision; retain its receipt and use the returned revision for later reads or writes.
- Verify a saved change with `morde_workspace_read` when available. If writing or read-back is unavailable, explain the limitation instead of claiming the list changed. Do not claim that a Morde Task or checklist creates a reminder or external task record.
