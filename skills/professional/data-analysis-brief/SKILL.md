---
name: data-analysis-brief
description: Use when the user asks for a data analysis plan, metric investigation, KPI explanation, dataset summary, dashboard brief, or analytical narrative. The workflow gathers business context, inspects available data sources or files when configured, asks clarifying questions, and produces a structured analysis brief with assumptions and next-step checks.
---

# Data Analysis Brief

Use this skill for analytical work where a useful answer depends on business context, metric definitions, available data, and uncertainty handling.

## Workflow

### 1. Frame The Analysis

Ask these questions before doing a deep analysis unless the user already answered them:

1. What decision or action should this analysis support?
2. What population, time period, geography, product, or segment is in scope?
3. Which metric definitions matter, and are there known caveats?
4. What data sources, uploaded files, knowledge bases, databases, or web sources should be used?
5. What would change the conclusion or make the analysis invalid?

If the user requests a quick answer, ask only the single most important missing question, then proceed with explicit assumptions.

### 2. Inspect Available Evidence

Use available tools based on the configured runtime:

- Use knowledge search or file context for reports, docs, and uploaded tables.
- Use database tools for bound database data sources.
- Use web search/fetch for current public facts when web tools are enabled.
- Use MCP tools only for their configured external systems.

Do not invent data. If data is unavailable, state the missing evidence and produce an analysis plan instead of fake numbers.

### 3. Produce The Brief

The final brief should contain:

1. Executive takeaway.
2. Data/evidence used.
3. Method and assumptions.
4. Findings, with numbers when evidence supports them.
5. Risks, gaps, and checks.
6. Recommended next action.

For complex tasks, ask the user to confirm the analysis frame before the final brief.

## Quality Rules

- Separate observed facts from inference.
- Name every important assumption.
- Highlight missing data that could reverse the conclusion.
- Prefer compact tables for metric comparisons.
- Avoid overconfident causal claims unless the data and method support them.
