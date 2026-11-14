---
author: Morde
name: office-recalculate
description: Recalculate spreadsheet formulas with an already available Host tool.
version: "1"
---

# Recalculate spreadsheet formulas

- Use only a recalculation tool already configured on the selected Host through this Task's `shell_exec`. Do not install a replacement, use an Agent Node CLI, or download tools.
- Keep the original workbook. Recalculate to a new file inside the Task Workspace and quote paths. If no supported Host tool is available or it fails, preserve the workbook and report that results could not be refreshed.
- Reopen the output with formula and cached-value views when the available library supports both. Confirm formulas survived and cached values match the requested logic and expected totals. Do not replace formulas with constants or describe stale/absent caches as fresh.
- Recalculation does not validate business logic or guarantee Excel-specific compatibility. Morde publishes the result only after the Host output is committed and byte-verified as a Task artifact.
