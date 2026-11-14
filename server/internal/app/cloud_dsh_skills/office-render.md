---
author: Morde
name: office-render
description: Render Office files for requested visual inspection using an already available Host tool.
version: "1"
---

# Office visual inspection

- Use only a renderer already configured on the selected Host and invoke it through this Task's `shell_exec`. Do not install a renderer, use an Agent Node CLI or sidecar, invoke a network package runner, or bypass a failed render with a second pipeline.
- Keep the final file and any requested render output inside the Task Workspace. For inspection-only previews, use a unique private temporary directory within that Workspace and remove only that directory after inspection. Never alter the Workspace root or source file to force a render.
- Render only the requested or changed pages, slides, sheets, or ranges. Inspect clipping, alignment, hierarchy, contrast, chart labels, and number formats as relevant. A successful render command alone does not establish that the document looks correct.
- If no approved Host renderer is available, complete available content checks and say visual inspection was not completed. Do not claim visual verification.
- Keep any requested final export in the Task Workspace. Morde publishes files only through the verified Task artifact path; previews are not deliverables unless the user asked for them.
