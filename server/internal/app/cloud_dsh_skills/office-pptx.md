---
author: Morde
name: office-pptx
description: Create, read, edit, and validate PowerPoint presentations on the selected Host.
version: "1"
---

# PowerPoint presentations

- Use `shell_exec` on the authorized Host and inside the Task Workspace. Use `python-pptx` only when it is already available in the Host's configured environment. Do not install packages or search Agent Node paths; report a missing dependency without altering the source.
- Inspect slide layouts, text runs, images, tables, and charts before editing. Change only requested content and preserve mixed formatting. Save a new presentation unless the user requested an in-place edit; rebuilding may discard animations, SmartArt, and unsupported extensions.
- Create editable text, tables, and charts. Keep image assets in the Task Workspace, use no network URLs, and set slide dimensions and chart data explicitly.
- Reopen the saved file and verify slide count, requested content, chart data, and preserved content. This does not establish visual fit; load `office-render` for requested visual inspection or appearance-focused edits.
- Keep scripts and output under this Task Workspace. Do not use `present` or external file delivery; Morde's verified Task artifact path publishes confirmed output.
