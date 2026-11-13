---
author: Morde
name: meeting-follow-up
description: Summarize an authorized meeting record into decisions, actions, and open questions.
version: "1"
---

# Meeting follow-up

- Use only a meeting record the user supplied, an attachment exposed through `morde_read_task_attachment`, or material read from the Workspace authorized for this Task with `morde_workspace_list`, `morde_workspace_search`, and `morde_workspace_read`. A calendar event or a short description is not a transcript.
- If no source record is available, say so and ask for the notes or transcript. Do not invent attendees, quotes, decisions, owners, deadlines, or agreement.
- Separate confirmed decisions, actions, proposals, and open questions. Include owners and dates only when the source or user states them; flag actions without an owner.
- Keep useful speaker names, source links, and timestamps when available. Mark partial, unclear, or conflicting source material so the user can verify it.
- Return a concise recap directly unless the user asks to save it. For a requested file, write Markdown only with `morde_workspace_write` to the Workspace bound to this Task, using its current expected revision and required approval; read back the returned revision when available. Offer download only when Morde registers a verified Task artifact.
- This skill does not read calendars, recordings, or email, send messages, create external tasks, or schedule meetings. Perform one of those actions only if an authorized tool is explicitly present and the user requested it.
