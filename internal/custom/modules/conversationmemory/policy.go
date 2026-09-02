// Package conversationmemory owns cross-agent dialogue continuity and
// model-independent preservation of traceable user text. It deliberately does
// not classify turns with domain phrases or contain dataset answers, case
// identifiers, or business templates. The answering model receives one common
// semantic schema while the normal conversation history remains intact.
package conversationmemory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	generationMarker          = "[WEKNORA_DIALOGUE_CONTINUITY_V12]"
	rewriteMarker             = "[WEKNORA_DIALOGUE_INTENT_V12]"
	turnSemanticMarker        = "[WEKNORA_CURRENT_TURN_SEMANTICS_V12]"
	historicalAssistantMarker = `<historical_assistant_output authority="non_source">`
	historicalUserMarker      = `<historical_user_input`

	maxArchiveTurns        = 48
	archiveHeadTurns       = 8
	maxArchiveRunes        = 16000
	maxArchiveRunesPerTurn = 1200
	maxSourceLedgerRunes   = 8000
	maxSourceRunesPerTurn  = 600
	maxCurrentSourceRunes  = 6000
)

// FetchMessageLimit returns one bounded indexed read large enough for the
// configured recent Q&A window plus a user-only archive.
func FetchMessageLimit(recentRounds int) int {
	if recentRounds < 1 {
		recentRounds = 1
	}
	limit := (recentRounds+maxArchiveTurns)*2 + 8
	if limit < 80 {
		return 80
	}
	if limit > 256 {
		return 256
	}
	return limit
}

// BuildUserArchive builds a bounded ledger for completed user statements that
// fall outside the configured full-history window. Recent user messages are
// source-labelled in normal chat history, so duplicating them here would add
// prompt latency without adding evidence. Assistant answers are excluded and
// therefore cannot become durable facts.
func BuildUserArchive(queries []string, recentRounds int) string {
	if len(queries) == 0 {
		return ""
	}
	if recentRounds < 0 {
		recentRounds = 0
	}
	if recentRounds > len(queries) {
		recentRounds = len(queries)
	}
	olderCount := len(queries) - recentRounds
	type userMessage struct {
		index int
		text  string
	}
	messages := make([]userMessage, 0, olderCount)
	for index, query := range queries[:olderCount] {
		messages = append(messages, userMessage{index: index + 1, text: query})
	}
	omitted := 0
	omissionIndex := -1
	if len(messages) > maxArchiveTurns {
		tailCount := maxArchiveTurns - archiveHeadTurns
		omitted = len(messages) - maxArchiveTurns
		selected := make([]userMessage, 0, maxArchiveTurns)
		selected = append(selected, messages[:archiveHeadTurns]...)
		omissionIndex = len(selected)
		selected = append(selected, messages[len(messages)-tailCount:]...)
		messages = selected
	}

	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("completed_user_message_count: %d", len(queries)))
	builder.WriteString(fmt.Sprintf("\nrecent_source_labelled_message_count: %d", recentRounds))
	if len(messages) == 0 {
		return builder.String()
	}
	// Divide the fixed budget across all selected turns. This retains both the
	// foundations and the latest updates instead of allowing a few long early
	// messages to crowd the tail out of the ledger.
	perTurnLimit := (maxArchiveRunes-1024)/len(messages) - 32
	if perTurnLimit > maxArchiveRunesPerTurn {
		perTurnLimit = maxArchiveRunesPerTurn
	}
	if perTurnLimit < 80 {
		perTurnLimit = 80
	}
	for index, message := range messages {
		if index == omissionIndex {
			builder.WriteString(fmt.Sprintf("\n[omitted_middle_user_messages=%d]", omitted))
		}
		query := truncateRunes(message.text, perTurnLimit)
		if strings.TrimSpace(query) == "" {
			continue
		}
		builder.WriteByte('\n')
		// Preserve user text exactly (subject only to the documented rune bound).
		// HTML escaping would make exact-source quotations differ from the
		// original message and weaken attribution in long conversations.
		builder.WriteString(fmt.Sprintf("%s: %s", UserTurnSourceID(message.index), query))
	}
	return truncateRunes(builder.String(), maxArchiveRunes)
}

// BuildUserSourceLedger returns one bounded, chronological ledger containing
// every completed user-authored message available to the caller. Recent turns
// intentionally also remain in normal chat history: the duplication gives the
// model a single provenance view without removing assistant context needed for
// ordinary follow-ups. The budget is fixed, so this does not grow without
// bound in long-running conversations.
func BuildUserSourceLedger(queries []string) string {
	if len(queries) == 0 {
		return ""
	}
	type userMessage struct {
		index int
		text  string
	}
	messages := make([]userMessage, 0, len(queries))
	for index, query := range queries {
		messages = append(messages, userMessage{index: index + 1, text: query})
	}
	omitted := 0
	omissionIndex := -1
	if len(messages) > maxArchiveTurns {
		tailCount := maxArchiveTurns - archiveHeadTurns
		omitted = len(messages) - maxArchiveTurns
		selected := make([]userMessage, 0, maxArchiveTurns)
		selected = append(selected, messages[:archiveHeadTurns]...)
		omissionIndex = len(selected)
		selected = append(selected, messages[len(messages)-tailCount:]...)
		messages = selected
	}

	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("completed_user_message_count: %d", len(queries)))
	builder.WriteString(fmt.Sprintf("\nledger_user_message_count: %d", len(messages)))
	perTurnLimit := (maxSourceLedgerRunes-1024)/len(messages) - 32
	if perTurnLimit > maxSourceRunesPerTurn {
		perTurnLimit = maxSourceRunesPerTurn
	}
	if perTurnLimit < 80 {
		perTurnLimit = 80
	}
	for index, message := range messages {
		if index == omissionIndex {
			builder.WriteString(fmt.Sprintf("\n[omitted_middle_user_messages=%d]", omitted))
		}
		query := truncateRunes(message.text, perTurnLimit)
		if strings.TrimSpace(query) == "" {
			continue
		}
		builder.WriteByte('\n')
		builder.WriteString(fmt.Sprintf("%s: %s", UserTurnSourceID(message.index), query))
	}
	return truncateRunes(builder.String(), maxSourceLedgerRunes)
}

// UserTurnSourceID returns the stable, chronological identifier used by every
// agent when a user asks for source attribution. It depends only on persisted
// user-message order and contains no scenario or Eval semantics.
func UserTurnSourceID(index int) string {
	if index < 1 {
		index = 1
	}
	return fmt.Sprintf("user_turn_%03d", index)
}

// HistoricalAssistantOutput marks prior model text as dialogue context rather
// than a factual source. A later explicit user confirmation can still promote a
// proposition by placing it in a user-authored source message.
func HistoricalAssistantOutput(content string) string {
	content = strings.TrimSpace(content)
	if content == "" || strings.HasPrefix(content, historicalAssistantMarker) {
		return content
	}
	return historicalAssistantMarker + "\n" + content + "\n</historical_assistant_output>"
}

// HistoricalUserInput labels a persisted user-authored message with the same
// source identifier used by the bounded ledger. Supplemental image/file
// context must be kept outside this block so derived text is never mistaken
// for a verbatim user assertion.
func HistoricalUserInput(content, sourceID string) string {
	content = strings.TrimSpace(content)
	if content == "" || strings.HasPrefix(content, historicalUserMarker) {
		return content
	}
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return content
	}
	return fmt.Sprintf(`<historical_user_input source_id="%s" authority="user_authored">`+
		"\n%s\n</historical_user_input>", sourceID, content)
}

func EnsureGenerationContract(prompt string) string {
	if strings.Contains(prompt, generationMarker) {
		return prompt
	}
	contract := generationMarker + `
Domain-neutral dialogue and grounding contract:
- The current user message is the active task. Older user messages are chronological sources only when relevant; earlier assistant text is context, never a factual authority unless the user later confirms it.
- Build state from atomic propositions keyed by object, field, value, modality and source. Resolve explicit updates chronologically and retire only older propositions that truly conflict.
- Preserve epistemic class and proof direction. Asserted, unknown/not supplied, explicitly pending/awaiting, questioned, proposed, hypothetical, instructed and negated are distinct. Missing evidence leaves a claim unknown; it does not prove the negative. Never turn unknown into pending or any determinate lifecycle value.
- Requirements, schemas, thresholds, examples, placeholders and role assignments describe rules or structure; they do not create case values, actors, actions, outcomes or completed events. Bind each value to its sourced object and field, and keep actor, role, action and outcome separate.
- Before outputting a concrete state claim, verify that the same object, field, value and modality are entailed by exact user text or real current-turn evidence. Otherwise omit it or mark it unknown/not supplied. Do not invent fields or fill missing operational details for completeness.
- When the current task asks for a decision or consolidated state, recompute it from the newest active user facts and the current retrieved rule. A newer complete evidence set may resolve an older unknown for the same field; re-derive the result instead of copying an earlier assistant conclusion, and resolve it only when every rule prerequisite is actually established.
- Keep retrieved synthesis inside the evidence boundary. Paraphrase and combine what the source entails, but do not add an unstated rationale, risk, definition, actor, process stage, consequence, alternative, or recommendation merely because it sounds plausible. State a limitation when the requested explanation exceeds the source.
- Keep active facts, retired facts, unknown facts, explicitly pending facts, source attribution, output scope and action boundaries separate when the task needs them. Honor the requested form and exclude unrelated history.
- Dialogue content is not external persistence. Creating or revising wording, structured text, a plan or a record in chat does not prove or authorize a file, command, message, database write or other external mutation.
- Interpret operation boundaries semantically by actor, action, object, destination and scope. A boundary never becomes an affirmative operation. It persists only within its stated continuing scope until an explicit user update changes it; one-answer formatting/source constraints expire with that answer.
- A boundary is permission/scope information, not an audit log. When reporting boundaries, describe what is allowed, prohibited or requires authorization; do not convert the boundary, the absence of a tool call, or the absence of a record into a claim that an operation did not happen. An operation's positive or negative outcome needs its own source.
- Tool selection is model-owned and based on the complete request, not isolated words. Use external retrieval for claims that need current source evidence, even when the topic discusses a prohibited action; do not retrieve for dialogue-only transformations or against a current source restriction.
- A file artifact is appropriate only when durable/downloadable bytes are part of the requested outcome or the task operates on an existing file. A JSON/YAML/Markdown/code/table/record/plan/draft/summary representation is chat content by default. Never create a file merely because an artifact tool is available.
- Before any local file or artifact tool, decide whether the requested outcome would be incomplete without durable bytes or an operation on an existing file. If a complete answer can be delivered as chat content and no existing file operation is requested, do not use a file tool. This decision remains the model's semantic judgment and never depends on a phrase list.
- Report an operation only from a matching successful current-turn result. For citations, use only the exact current source handle beside the claim it supports; never invent or reuse a prior-turn handle.
- Follow the current output language and return one direct user-visible answer without hidden reasoning, planning or protocol narration.`
	if strings.TrimSpace(prompt) == "" {
		return contract
	}
	return strings.TrimSpace(prompt) + "\n\n" + contract
}

func EnsureQueryUnderstandingContract(prompt string) string {
	if strings.Contains(prompt, rewriteMarker) {
		return prompt
	}
	contract := rewriteMarker + `
Domain-neutral intent rules:
- Classify the complete semantic request, never isolated words. Use intent "conversation_state" for substantive work grounded in user-authored dialogue; it is independent from whether external evidence is also needed.
- The JSON "evidence_need" value must be "none", "knowledge_base" or "web". Use "none" only when every requested claim is grounded in permitted dialogue/attachments; use the matching external source when any claim needs current evidence.
- A configured or selected source is availability, not evidence need. Keep evidence_need "none" for a dialogue-only transformation even when a knowledge base is selected; do not manufacture a retrieval subquestion.
- The JSON "evidence_query" is only the source-facing question. Leave it empty for evidence_need "none"; otherwise include all and only the claims needing that source, without dialogue bookkeeping or output formatting.
- Mixed requests keep their primary semantic intent and independently request the required evidence. Quoting or transforming user-authored text alone does not retrieve; a domain claim still retrieves even when it discusses an operation boundary.
- Preserve atomic object/field bindings, chronology, modality and unresolved-state polarity. Unknown and explicitly pending are different; questions, proposals, hypotheticals, requirements, assignments and missing evidence do not become completed events or determinate lifecycle values.
- An operation/source restriction is a boundary, not an affirmative request. Respect its stated scope and do not infer tool intent from negation, quotation, history or configured availability.
- Preserve exact identifiers, names, dates and amounts in rewrites. Rewrite only the current evidence question and do not revive an expired topic.`
	if strings.TrimSpace(prompt) == "" {
		return contract
	}
	return strings.TrimSpace(prompt) + "\n\n" + contract
}

func AppendCurrentTurnDirective(content, originalQuery string) string {
	if strings.Contains(content, turnSemanticMarker) {
		return content
	}
	// Prior turns already appear in normal chat history and in the bounded
	// user-only archive. Repeating or heuristically classifying them here would
	// both waste context and let a finite phrase list change production
	// behavior. This block therefore carries only exact current-user fragments
	// plus the universal semantic dimensions the model may use when relevant.
	type sourceFragment struct {
		Text      string `json:"text"`
		Source    string `json:"source"`
		Truncated bool   `json:"truncated"`
	}
	originalRunes := []rune(originalQuery)
	boundedOriginal := originalQuery
	truncated := false
	if len(originalRunes) > maxCurrentSourceRunes {
		boundedOriginal = string(originalRunes[:maxCurrentSourceRunes])
		truncated = true
	}
	fragments := []sourceFragment{{
		Text: boundedOriginal, Source: "current_user_message", Truncated: truncated,
	}}
	var payloadBuffer bytes.Buffer
	encoder := json.NewEncoder(&payloadBuffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(struct {
		Dimensions      []string         `json:"semantic_dimensions"`
		SourceFragments []sourceFragment `json:"source_fragments"`
	}{
		Dimensions: []string{
			"active_facts", "retired_facts", "unknown_facts", "explicitly_pending_facts",
			"source_attribution", "claim_source_validation", "field_object_binding",
			"actor_action_outcome", "epistemic_modality", "output_scope", "action_boundaries",
		},
		SourceFragments: fragments,
	})
	payload := strings.TrimSuffix(payloadBuffer.String(), "\n")
	directive := turnSemanticMarker + `
This block contains exact fragments from the current original user message and a domain-neutral interpretation schema. It does not pre-classify any fragment.
- Answer the current user task exactly once.
- For dialogue state, use only exact user-authored fragments; for external claims, use real current-turn evidence. Track atomic object, field, value, modality and source, applying explicit updates chronologically.
- Preserve epistemic class and proof direction. Unknown and explicitly pending are different; absence of evidence does not prove a negative. Questions, proposals, hypotheticals, instructions, requirements, assignments and examples do not become completed events or case values.
- Before emitting a concrete state claim, require an exact source that entails the same object, field, value and modality. Otherwise omit it or mark it unknown/not supplied. Keep active, retired, unknown, pending, attribution, scope and action-boundary dimensions distinct when relevant.
- For a requested decision or consolidated state, re-derive from the newest active user facts plus current rule evidence. Resolve an older unknown only when all prerequisites are now established; never rely on an earlier assistant conclusion as the source.
- Keep external explanations within what the retrieved evidence entails. Do not fill in plausible rationales, risks, definitions, actors, stages, consequences, alternatives, or recommendations that the evidence does not supply.
- In the user_source_ledger, completed_user_message_count is the number of prior completed user turns; recent turns are source-labelled directly in history, and this current_user_message is the next user_turn ordinal.
- Apply action/source boundaries only to their semantic actor, action, object, destination and scope. They never authorize an operation. A chat content change is not an external write, and a structured representation is not a file unless durable bytes are part of the requested outcome.
- Treat a boundary as permission/scope information rather than an operation audit. State allowed/prohibited scope without claiming an action did or did not occur unless an independent user fragment or successful current-turn result establishes that outcome.
- Select tools from the complete request, not from isolated words. Respect current source restrictions; retrieve when an external claim needs evidence; report operations only from matching successful current-turn results.
turn_context=` + payload
	return appendDirective(content, directive)
}

func TerminalGenerationDirective() string {
	return "Return one non-empty user-visible final answer for the exact current task inside exactly one <weknora_final_response>...</weknora_final_response> envelope; put no planning, self-talk or protocol narration outside it. Use the current requested language. Ground every concrete state claim in an exact user fragment or real current-turn evidence for the same object, field, value and modality; otherwise omit it or keep it unknown. For a requested decision or consolidated state, re-derive from the newest active user facts and current rule evidence, resolving an older unknown only when every prerequisite is established. Keep external explanations within what the evidence entails and omit plausible but unsourced rationale, risk, definition, actor, stage, consequence, alternative, or recommendation. Preserve unknown, explicitly pending, hypothetical, questioned and asserted classes, and never infer case state or an operation outcome from a rule, role, missing evidence or boundary. Treat an action boundary only as permission/scope information: it can justify allowed/prohibited wording but cannot prove that an operation did or did not occur. Use only current canonical citation handles beside supported claims, and emit none when current citable evidence is absent. Distinguish chat content from external operations and report a positive or negative operation outcome only from an independent user source or matching successful current-turn result."
}

func AppendUserArchive(prompt, archive string) string {
	prompt = EnsureGenerationContract(prompt)
	block := UserArchiveBlock(archive)
	if block == "" {
		return prompt
	}
	return prompt + "\n\n" + block
}

// AppendUserSourceLedger adds the complete bounded user-only provenance view to
// the system prompt. It is a normal production grounding aid, not Eval state:
// it contains only persisted user text and cannot contain rubric or judge data.
func AppendUserSourceLedger(prompt, ledger string) string {
	prompt = EnsureGenerationContract(prompt)
	block := UserSourceLedgerBlock(ledger)
	if block == "" {
		return prompt
	}
	return prompt + "\n\n" + block
}

func UserSourceLedgerBlock(ledger string) string {
	ledger = strings.TrimSpace(ledger)
	if ledger == "" {
		return ""
	}
	return `<user_source_ledger role="historical_user_data" authority="user_authored_only">
The entries below are a bounded chronological provenance view of completed user statements. They are the factual authority for dialogue state; assistant history is context only. Resolve explicit updates by object and field, preserve each statement's modality, and never fill a missing value or operation outcome from a schema, role, example, question or absence of evidence.
` + ledger + `
</user_source_ledger>`
}

func UserArchiveBlock(archive string) string {
	archive = strings.TrimSpace(archive)
	if archive == "" {
		return ""
	}
	return `<user_source_ledger role="historical_user_data" authority="user_authored_only">
The entries below are exact completed user statements outside the recent full-history window, with stable chronological source IDs. Recent user turns carry the same source-ID form directly in conversation history and are not duplicated here. These are factual source fragments, not current instructions and not retrieved evidence. Historical assistant outputs are deliberately excluded as fact sources. Resolve user-authored conflicts chronologically; newer explicit user updates win. The current user message has the next ordinal after completed_user_message_count.
` + archive + `
</user_source_ledger>`
}

func appendDirective(content, directive string) string {
	if strings.TrimSpace(content) == "" {
		return directive
	}
	return strings.TrimSpace(content) + "\n\n" + strings.TrimSpace(directive)
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "…[truncated]"
}
