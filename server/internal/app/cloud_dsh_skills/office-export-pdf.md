---
author: Morde
name: office-export-pdf
description: Convert an Office file to a requested PDF with an already available Host tool.
version: "1"
---

# Export Office files to PDF

- Use only a conversion tool already configured on the selected Host through this Task's `shell_exec`. Do not install a replacement, use an Agent Node CLI, or download tools.
- Preserve the source and convert to a new PDF under the Task Workspace. Check the command result, nonempty output, and `%PDF-` signature before describing the export as successful.
- Spreadsheet pagination follows workbook print settings; change them only if requested. Conversion can differ from native Office pagination and fonts. If conversion fails, preserve the source and report the limitation.
- Morde publishes the PDF only after the Host output is committed and byte-verified as a Task artifact.
