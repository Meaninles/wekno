package generalagent

// Package generalagent implements the custom "general-agent" runtime.
//
// The large agentic loop runs in custom/services/general-agent via Claude Agent
// SDK. This Go module owns the WeKnora-side contract: runtime config
// resolution, native tool/MCP/Skills registry reuse, human approval events,
// artifact persistence and download routes.
