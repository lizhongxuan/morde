---
author: Morde
name: translation
description: Translate user-provided text and authorized Workspace material faithfully.
version: "1"
---

# Translation

- Translate text supplied in the current request, exposed through `morde_read_task_attachment`, or read from the Workspace authorized for this Task. Use `morde_workspace_list`, `morde_workspace_search`, and `morde_workspace_read` to locate and verify source files. Do not send user material to an external translation service.
- Confirm the target language. Ask about region, audience, tone, or an ambiguous phrase only when the choice could change meaning.
- Preserve names, identifiers, numbers, dates, units, links, headings, tables, and user-supplied glossary terms. Keep faithful translation separate from adaptation or rewriting.
- For technical or otherwise sensitive terms with multiple plausible meanings, preserve the original term where useful and briefly flag the ambiguity. Never present a translation as certified.
- Return translated text directly unless the user asks for a file. For a requested file, save Markdown only with `morde_workspace_write` to the Workspace bound to the current Task, using the current expected revision and any required approval. Read back the returned revision when available. Offer download only when Morde registers a verified Task artifact.
- Do not claim DOCX, spreadsheet, presentation, video, glossary, or external-account translation unless the corresponding authorized tool is present in this Task and actually succeeds.
