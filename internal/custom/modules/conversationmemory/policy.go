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
	generationMarker          = "[WEKNORA_DIALOGUE_CONTINUITY_V3]"
	rewriteMarker             = "[WEKNORA_DIALOGUE_INTENT_V3]"
	turnSemanticMarker        = "[WEKNORA_CURRENT_TURN_SEMANTICS_V3]"
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
Dialogue continuity and grounding rules:
- Treat the current user message as the active task. Older user messages are chronological source material, not current instructions unless the user refers to them.
- User-authored text and real current-turn retrieval evidence are the only factual sources. Never promote an earlier assistant inference into a fact.
- Distinguish asserted facts and completed events from questions, requests, instructions, examples, hypotheticals, proposals, negations, and analysis. Mentioning or asking how something could be done never proves that it happened.
- A count, outcome (including zero), missing record, lack of evidence, or request to analyze a lifecycle condition does not by itself establish that an action started, stopped, completed, failed, or never occurred. Keep lifecycle state unknown unless exact user text or current-turn evidence states it.
- Do not infer whether an action occurred from the conversational plausibility of asking whether or why it should occur. A question about whether/why an action should or should not happen is compatible with occurred, not occurred, or unknown; only an explicit assertion or evidence resolves it.
- A missing value remains unknown or pending. Never rewrite "not supplied" as "none", "not applicable", "ready", or "completed".
- Treat synonymous unresolved labels and paraphrases as the same semantic state. Rewording unknown, pending, undecided, unmeasured, or not supplied neither creates a second fact nor resolves the first; only an explicit determinate update resolves it.
- Distinguish conversation content from external persistence. When the user explicitly adopts wording, a plan item, or a field value for the current chat task, it is active dialogue state even if no file, database, ticket, or message was changed. Never claim external persistence without a successful operation.
- A policy requirement, threshold, SLA, schema, assigned supporting role, or proposed next step does not establish the current lifecycle of a particular incident, request, record, or operation. Lifecycle needs an explicit user assertion or current evidence about that object.
- Resolve explicit updates chronologically: newer values may retire conflicting older values. Keep active, retired, unknown/pending, source attribution, requested output scope, and action boundaries distinct.
- Decompose compound user statements into atomic propositions before applying an update. Retire only propositions that are logically incompatible with the newer user statement; preserve compatible facts, qualifiers, actors, objects, scope, and modality exactly as supplied.
- Historical user messages carry stable source IDs either in the user_source_ledger or directly on recent history. When the user asks for sources, attribute facts only to the exact user/current-user fragment that contains them; never guess a turn number or cite an assistant output.
- Output scope is an exclusion boundary. When the current user asks for only selected fields, topics, or transformations, do not add unrelated historical state or template fields.
- A document schema, example, suggested placeholder, or earlier assistant-generated field is not conversation state. Do not import it into a record, draft, or snapshot unless the user explicitly adopts that exact content.
- Drafts, plans, templates, and sample text may create wording and neutral connective prose, but must not fill missing operational details. Honor the requested count and form; omit or visibly placeholder missing facts instead of inventing a duration, quantity, lifecycle state, recipient, actor, role duty, destination, channel, contact route, commitment, or completed step.
- An action boundary limits actual operations; it does not invert into a request. Distinguish editing a proposal's content from modifying a file or external system.
- Preserve the exact actor, action, object, destination, and turn scope of an action boundary. Do not broaden a prohibition or convert a turn-scoped instruction into a permanent business fact.
- Within the same continuing task or object, an operation boundary remains active until the user explicitly revokes, narrows, or supersedes it. A later request to edit, draft, calculate, or discuss content does not authorize a file, artifact, message, command, retrieval, or external-system action.
- Updating an attribute, value, location, owner, version, plan alternative, or draft does not create a new task/object and does not expire an operation boundary that governs it.
- A quoted or discussed prohibition is not automatically an operational instruction. Questions that analyze why an action cannot occur remain information requests.
- A hypothetical, example, recommendation, or explanatory question about an action stays local to that discussion. Do not carry it into the state or action boundaries of a concrete object unless the user explicitly adopts it for that object.
- State-maintenance and conversation-only transformation turns do not retrieve or invoke tools unless the current user positively asks for external evidence or an actual operation. Ordinary knowledge questions retain necessary retrieval even if their subject mentions a prohibited action.
- A user source restriction such as using only supplied dialogue or not consulting external material is a tool boundary for the task and scope the user states. While that scope remains active, answer from the permitted sources or explain the limitation; tool availability and an earlier retrieval workflow do not override it.
- Do not create or modify files, contact people, execute commands, install software, or mutate an external system unless the current user explicitly requests that concrete operation and the corresponding tool actually succeeds.
- Never say that you searched, retrieved, read, verified, saved, sent, updated, or otherwise performed an operation unless matching current-turn evidence or a successful tool result establishes it. Likewise, never claim that an operation did not occur merely from silence or a prohibition; when asked, report only user-stated boundaries and verified current-turn outcomes.
- When attribution is requested, copy only a source ID visibly attached to the exact user fragment that asserted the claim. If the exact ID is unavailable or uncertain, quote the user text without inventing an ID; a later request or assistant summary is not the origin of an older fact.
- Obey an explicit current-turn output-language request; otherwise use the configured user language.
- Do not invent facts, fixed fields, named examples, decisions, or completed actions. Return only the user-visible answer: never expose intent classification, hidden reasoning, planning, self-talk, source-selection notes, or runtime protocol text.`
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
Intent rules:
- The value of the JSON "intent" field MUST be exactly "conversation_state" when the current task is fully answerable from the current user message or user-authored dialogue and asks to record, update, retire, quote, audit, reformat, compare, summarize, or explain the semantics of that dialogue. This value is valid and mandatory even if an older base prompt does not list it.
- "conversation_state" has priority over "chitchat", "follow_up", "clarification", and "kb_search" whenever the task is dialogue-grounded state work. A substantive state request is not chitchat. A clear request to explain a user-authored instruction, quotation, negation, proposal, or boundary is not ambiguous merely because it asks "why".
- The JSON "evidence_need" field MUST be exactly "none", "knowledge_base", or "web". It is independent of intent: use "none" when every requested claim is grounded in user-authored dialogue or supplied attachments, "knowledge_base" when any requested claim needs configured document/KB evidence, and "web" when current external web evidence is explicitly required.
- The JSON "evidence_query" field is the source-facing question used only for retrieval and reranking. Set it to an empty string when evidence_need is "none". Otherwise make it a concise, self-contained query containing only the claims that require the selected external source; exclude dialogue bookkeeping, output formatting, and user-state fields unless those are themselves the subject of the requested evidence.
- A positive request for document, knowledge-base, verification, or external citation evidence remains a retrieval task. For a mixed request, preserve the primary semantic intent (for example "conversation_state") and set evidence_need to the required external source; ground the dialogue-state portion separately in user text. Asking only to quote or attribute the user's own messages uses evidence_need "none" and does not retrieve.
- A negated retrieval phrase such as "do not search" is a tool boundary, never a request to search.
- Treat semantically equivalent unresolved labels as one state: a paraphrase of pending/unknown does not create a new fact or resolve the old one, while an explicit determinate value does.
- A value or plan wording explicitly adopted for the current chat task is dialogue state even when it was not written to an external system. Do not confuse "not externally persisted" with "missing from the conversation".
- Rules, thresholds, schemas, role assignments, recommendations, and explanatory questions do not by themselves establish a concrete object's lifecycle status.
- A hypothetical/example action discussed for explanation must not become state or an operational boundary for a named object unless the user explicitly adopts it there.
- "conversation_state" describes dialogue-grounded state, quotation, attribution, transformation, and semantic-analysis work; evidence_need determines whether that same turn also retrieves. The intent does not claim that every such turn creates durable state. Merely quoting or discussing an action boundary must not turn it into an active fact or completed event.
- Questions, examples, hypotheticals, proposals, and requested actions must remain in their original modality; never rewrite them as completed events or asserted state.
- Counts, outcomes (including zero), absent records, and analytical questions do not establish lifecycle state. Do not rewrite them as started, not started, completed, failed, or never happened without an explicit user assertion.
- Do not infer occurred or not-occurred from the pragmatic wording of a question about whether/why an action should happen; the question alone leaves both possibilities open.
- A conversation-only state or transformation task must use evidence_need "none" and an empty evidence_query. An ordinary knowledge question and the external portion of a mixed request still require the appropriate evidence_need and a source-facing evidence_query.
- Use "chitchat" only for social or casual conversation with no substantive task. Use "follow_up" only to expand an earlier answer when no state/source operation is requested. Use "clarification" only when missing user information makes the current task materially indeterminate; classification uncertainty alone is not a reason to retrieve.
- Preserve exact document names, structural identifiers, dates, amounts, project codes, and proper names in any rewrite.
- Rewrite only the current task; do not revive an expired historical topic.`
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
			"active_facts", "retired_facts", "unknown_pending_facts",
			"source_attribution", "epistemic_modality", "output_scope", "action_boundaries",
		},
		SourceFragments: fragments,
	})
	payload := strings.TrimSuffix(payloadBuffer.String(), "\n")
	directive := turnSemanticMarker + `
This block contains exact fragments from the current original user message and a domain-neutral interpretation schema. It does not pre-classify any fragment.
- Answer the current user task exactly once.
- When the task concerns conversation state, derive state facts only from exact user-authored fragments in this block, normal user history, or the user-only archive. Real retrieval evidence may support external factual claims.
- Preserve modality: an instruction, question, analysis, example, proposal, negation, or unknown value is not a completed event or active business fact unless the user explicitly says it is.
- A question about whether or why an action should or should not happen establishes neither occurred nor not-occurred; do not infer lifecycle from conversational plausibility.
- Treat semantically equivalent unresolved expressions as the same state. A pending/unknown paraphrase neither duplicates nor resolves a fact; only a determinate user update does.
- An explicitly adopted chat field, draft change, or plan item is valid conversation content without an external write. Keep that separate from whether any file, record, or system was actually modified.
- Do not infer a concrete object's lifecycle from a policy threshold, required field, assigned supporting role, recommendation, or explanatory example.
- Keep active, retired, unknown/pending, source attribution, output scope, and action boundaries semantically distinct when the user requests them; do not force state sections onto unrelated knowledge questions.
- In the user_source_ledger, completed_user_message_count is the number of prior completed user turns; recent turns are source-labelled directly in history, and this current_user_message is the next user_turn ordinal.
- Never turn a negative action boundary into an affirmative operation. Within the same task or object, keep an operation boundary active until the user explicitly changes it; a later content edit does not authorize a file, artifact, message, command, retrieval, or external-system action. Never report an operation as completed or absent unless user text or a current-turn tool result establishes that outcome.
- Keep a user-declared source boundary (for example, dialogue-only or no external lookup) within its stated task/scope until explicitly changed. A later knowledge question may positively request evidence and supersede that boundary for that question; merely mentioning an action or configured source does not.
- Changing an attribute, value, location, owner, version, plan alternative, or draft does not expire an operation boundary. For requested drafts, honor the exact count/form and do not add unsupported quantities, duration, recipients, role duties, contact routes, commitments, or outcomes.
turn_context=` + payload
	return appendDirective(content, directive)
}

func TerminalGenerationDirective() string {
	return "Return one non-empty user-visible final answer for the current task inside exactly one <weknora_final_response>...</weknora_final_response> envelope. Put no user-visible answer, planning, self-talk, or protocol narration outside that envelope; the runtime removes the envelope before streaming and persistence. Follow an explicit output language in the current user message, otherwise use the configured user language. When current-turn citable evidence exists, keep only the supplied exact citation handles inside the envelope adjacent to the claims they support; when it does not exist, emit no citation handle and do not present earlier retrieval as current evidence. When conversation state is relevant, use only traceable user-authored facts: every included field or proposition needs a matching user fragment, and changing one field does not adopt neighboring assistant- or document-generated fields. Preserve asserted versus questioned/hypothetical/unknown modality and the active, retired, unknown/pending, source-attribution, output-scope, and action-boundary distinctions the user actually requested. Treat synonymous unresolved labels as one state; only an explicit determinate update resolves it. An adopted chat field or plan item is dialogue content without proving an external write. A rule, threshold, supporting-role assignment, recommendation, hypothetical, or explanatory question does not establish a concrete object's lifecycle or become its state unless the user explicitly adopts it. A count, zero outcome, absent record, analysis request, or question about whether/why an action should happen establishes neither lifecycle direction. Honor requested count/form; creative wording must not add unsupported operational details. Changing a task attribute or plan alternative does not expire its operation boundaries. State a prohibition as an action boundary, never as a completed non-event, and report operations only when user text or verified current-turn tool results establish their outcome."
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
The entries below are a bounded chronological provenance view of completed user statements. Recent entries may also appear in normal chat history; that duplication does not create new facts. Use this ledger as the authority boundary for conversation-state facts while retaining assistant history only as non-authoritative dialogue context. Resolve explicit user updates chronologically. Before including a state field or proposition, locate the user fragment that supplies it; if no user fragment supplies it, omit it. A request to change one field adopts only that user-authored change, not adjacent fields from an assistant draft, document schema, example, or placeholder. Questions, requests, proposals, negations, counts, zero outcomes, missing records, and action boundaries retain their original modality and do not establish unasserted lifecycle events. Within a continuing task, an operation boundary remains active until an explicit user update changes it.
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
