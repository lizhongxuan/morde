---
author: Morde
name: app-creation
description: Create a small application inside the current Task's authorized Workspace.
version: "1"
---

# App creation

- Clarify the app's users, main job, and success criteria when they affect what to build. Inspect the current authorized Workspace before creating a parallel project.
- Keep source files in the Workspace bound to this Task. Use `morde_workspace_list`, `morde_workspace_search`, `morde_workspace_read`, and `morde_workspace_write` for its text files and current revisions.
- Run or preview code only when this Task actually provides authorized Host tools such as `shell_exec`, `file_read`, `file_write`, `file_list`, `file_find`, `browser_open_tab`, and `browser_snapshot`. These tools are scoped to the selected Host Workspace. Do not invent paths or assume a computer is available.
- Use dependencies and tools already present. Do not install packages, deploy, publish, or send source to an external service unless the user explicitly requests the action and the current Task has the required authorized tool and approval.
- Run the relevant checks when an authorized execution tool is available. If only Workspace text tools are available, deliver the source and clearly state that execution, preview, and tests were not run.
- Preserve existing project structure and unrelated files. Verify saved source by reading the returned Workspace revision when possible; report completion only for operations that actually succeeded.
