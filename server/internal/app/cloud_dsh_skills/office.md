---
author: Morde
name: office
description: Route Office work to the smallest relevant workflow for this Task's authorized Host.
version: "1"
---

# Office workflow

- This skill requires an explicitly selected Host with `shell_exec`; it grants no permission. Use only the Host tools listed for this Task and the Task Workspace mounted for that tool. Never use an Agent Node path, an unselected computer, or a path outside the authorized Workspace.
- Load only the matching skill with `nanomuse_skill_read`: `office-docx` for Word, `office-pptx` for PowerPoint, and `office-xlsx` for Excel. Load `office-render` for requested visual inspection or appearance-focused edits, `office-recalculate` when refreshed formula values are requested, and `office-export-pdf` for PDF conversion. Do not load irrelevant skills or reload a skill already read.
- Use only dependencies and tools already configured on the selected Host. Do not install packages, invoke network downloads, or copy files to an external service. If a required dependency or Host tool is unavailable, state that limitation and preserve the source.
- Keep scripts, intermediates, and outputs inside the Task Workspace. Use a new output file unless the user requested an in-place edit. Reopen and verify the saved result. Report visual rendering or recalculation as unavailable when the needed Host tool is absent; do not claim it was checked.
- Morde publishes a downloadable artifact only after the Host output is independently captured from the fixed Task Workspace view and committed through the verified artifact path. Do not claim that a file is delivered before the Task result confirms it.
