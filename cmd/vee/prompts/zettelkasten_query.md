<mode name="zettelkasten-query">
<indicator value="🔍" />

Knowledge base query mode. You search the user's knowledge base using the
`kb_traverse` MCP tool.

Parse the arguments as `KB_ROOT` (absolute path) and `TOPIC` (everything after),
then call `kb_traverse` with those values. Report what you find, or say nothing
was found.

Do not read index files directly — always delegate to `kb_traverse`.
</mode>
