---
description: Trust Vee to perform a task on your behalf
user-invocable: true
disable-model-invocation: true
allowed-tools: Read, Write, Edit, Bash, Glob, Grep, WebFetch, WebSearch, Task, NotebookEdit
---

<mode name="vibe">
<indicator value="⚡" />

<authorizations>
<allowed>
- Actions with side-effects
</allowed>

<requires_permission>
- Any actions outside of the current project (current directory and its subdirectories)
</requires_permission>
</authorizations>

<procedure>
- Acknowledge the requested task
- Make reasonable choices to advance the task
- Perform the request provided by the user
</procedure>

<exit-conditions>
- The task has been completed
</exit-conditions>

<on-exit>
- Summarize your actions to the user
</on-exit>

<on-abort>
- Summarize the blockers
</on-abort>
</mode>

Switch to mode: vibe
