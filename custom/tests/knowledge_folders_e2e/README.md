# Knowledge-folder API E2E

`api_e2e.ps1` runs against a live WeKnora backend. It covers authentication,
multi-level CRUD, concurrent duplicate protection, folder uploads, recursive
precomputed statistics, folder/document search, flat document-only semantics,
document and subtree moves, safe deletion, pagination, cross-KB isolation,
SSRF rejection, parallel reads, and (when the configured model pipeline is
available) real keyword retrieval of a document stored in a nested folder.

```powershell
pwsh ./custom/tests/knowledge_folders_e2e/api_e2e.ps1
```

By default the script creates and then removes its own knowledge base. Use
`-KeepData` to retain the fixture for browser E2E, or pass
`-Username`, `-Password`, and `-KnowledgeBaseId` to reuse an existing fixture.
