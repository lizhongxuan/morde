---
author: Morde
name: office-xlsx
description: Read, create, edit, and validate Excel workbooks on the selected Host.
version: "1"
---

# Excel workbooks

- Use `shell_exec` on the authorized Host and inside the Task Workspace. Use `openpyxl` or `pandas` only when already available in the Host's configured environment. Do not install packages or search Agent Node paths; report missing dependencies without altering the source.
- Inspect sheet names, cell types, formulas, styles, merged ranges, and referenced ranges before editing. Load formulas with `data_only=False` when they must survive. Use pandas for analysis, not as a round trip over a workbook that must retain sheets, formulas, charts, or formatting.
- Preserve numbers, dates, booleans, and identifiers as their intended types. Save a new workbook unless the user requested an in-place change. Do not rename XLS, XLSB, encrypted, or macro-enabled files to XLSX; do not claim that macros ran.
- Reopen the saved workbook and verify requested sheets, values, types, formulas, styles, and unchanged content. A formula write does not recalculate cached results. Load `office-recalculate` when refreshed values are requested; load `office-render` for requested visual inspection or appearance-focused changes.
- Keep scripts and output under this Task Workspace. Do not use `present` or external file delivery; Morde's verified Task artifact path publishes confirmed output.
