---
author: Morde
name: office-docx
description: Create, read, edit, and validate Word documents on the selected Host.
version: "1"
---

# Word documents

- Use `shell_exec` on the authorized Host and inside the Task Workspace. Use `python-docx` only when it is already available in the Host's configured environment. Do not install packages or search Agent Node paths. If the dependency is missing, explain the limitation instead of writing a fake DOCX with a text tool.
- For text edits, find the requested paragraph or runs and preserve their formatting. Rebuilding an existing DOCX can discard unsupported features; save a new file unless the user requested an in-place change.
- Use paragraph styles for headings and body text. For CJK text, set an East Asian font explicitly when needed. Account for merged/nested tables and preserve unrelated content.
- Reopen the saved DOCX with the same available library and verify the requested text and relevant formatting. Load `office-render` for requested visual inspection or appearance-focused changes; rendering is separate from structural validation.
- Keep scripts and output under this Task Workspace. Do not use `present` or external file delivery; Morde's verified Task artifact path publishes confirmed output.
