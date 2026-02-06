---
description: Record profile feedback (positive or negative example)
user-invocable: true
allowed-args: true
---

The user wants to record feedback about the current profile's behavior.
Their input: `$ARGUMENTS`

Your job is to collaborate with the user to craft a concise, actionable
example or counter-example statement that will guide future sessions.

1. Read the user's input and determine whether this is positive ("good")
   or negative ("bad") feedback. If ambiguous, ask.
2. Draft a concise statement (1-2 sentences) that captures the essence
   of what should be done (good) or avoided (bad).
3. Present the draft to the user and iterate until they're satisfied.
4. Ask whether this should apply to all projects ("user") or just this
   project ("project").
5. Once the user confirms, call `feedback_record` with the finalized
   kind, statement, and scope.
