# WeKnora Document Processing Agent Sidecar

This service runs the same Claude Agent SDK app as the general agent, but with a document-processing image that preloads LibreOffice, Pandoc, PDF utilities, Chinese fonts and common Word/Excel/PDF/PPT Python libraries.

Only `agent_type=document-processing-agent` should route to this sidecar. General agents continue to use `weknora-custom-general-agent`.
