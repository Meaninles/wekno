// Package conversationmemory owns cross-agent dialogue continuity and
// model-independent preservation of traceable user text. It deliberately does
// not classify turns with domain phrases or contain dataset answers, case
// identifiers, or business templates. The answering model receives one common
// semantic schema while the normal conversation history remains intact.
package conversationmemory

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	generationMarker          = "[WEKNORA_DIALOGUE_CONTINUITY_V15]"
	rewriteMarker             = "[WEKNORA_DIALOGUE_INTENT_V15]"
	turnSemanticMarker        = "[WEKNORA_CURRENT_TURN_SEMANTICS_V15]"
	historicalAssistantMarker = `<historical_assistant_output authority="non_source" factual_authority="none" current_evidence_required_if_reused="true">`
	historicalUserMarker      = `<historical_user_input`

	maxArchiveTurns = 48
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
- The current user message is the only active task. Older user messages are chronological fact sources when relevant; assistant history is context, never evidence unless the user later confirms a claim.
- Before answering, classify every requested proposition by authority. Dialogue facts require exact user text; document/domain facts require claim-bearing evidence retrieved in this turn. If a requested external claim exists only in assistant history, retrieve it again. If every requested proposition is dialogue-grounded, do not retrieve.
- Represent state as atomic object/field/value/modality/source propositions. Apply explicit updates chronologically, retire only true conflicts, and keep asserted, unknown, explicitly pending, questioned, proposed, hypothetical and negated classes distinct. Absence of evidence proves neither a positive nor a negative outcome.
- Keep speech-act modality separate from proposition polarity. Use this three-valued calculus: assert(P) licenses P; assert(not-P) licenses not-P; constrain(output, P) licenses neither polarity. not-assert(P) is not assert(not-P). Without a separate source, do not lexicalize it as P, not-P, not-yet-P, rejected-P or incomplete-P. If the field must be emitted, preserve only the user's exact unresolved class.
- Runtime clocks, locale, configuration, source selection, schemas, roles, examples and placeholders do not supply business values or events. A current/latest label selects the newest active user value for the same discourse field, never the runtime date or a neighboring field.
- Resolve an omitted or anaphoric object to the most recent compatible user-authored discourse object. Do not jump to an older topic, retrieved document subject or assistant-generated object unless the current user explicitly returns to it.
- User facts and current evidence are closed sets. Omit unsupported values and adjacent workflow details. An unknown-items section may contain only fields explicitly unresolved in user text or named by the requested bounded schema; do not turn every absent field into a gap.
- Rewrites, translations, summaries and handoffs may reorganize or omit source material but must not add or strengthen prerequisites, compatibility, procedures, dates, consequences or recommendations. Recompute decisions from current facts and current rule evidence instead of copying an earlier assistant conclusion.
- Interpret action boundaries by actor/action/object/destination/scope. A boundary is permission information, not proof that an operation did or did not occur; chat content is not a file or external action. Report an outcome only from an explicit user assertion or a matching successful current-turn result.
- Tool choice is model-owned and follows the complete semantic request, never isolated words. Before any call, verify that the exact requested outcome needs that tool's effect and that every required argument is sourced. A complete chat answer needs no workspace, artifact or operation tool. Use the smallest sufficient evidence path, stop when claim coverage is complete, and deep-read only when returned evidence is truncated, ambiguous, context-dependent or lacks a current citation handle.
- Citation handles are request-local. A fragment supports only claims about the same named object, field, relation, value and modality; shared platform, action or vocabulary does not merge different subjects. Place only a matching current handle beside the claim it supports; when the user requests item-by-item citations, repeat that handle beside every supported item even if they share one source. Finish as ordinary assistant text—there is no final-answer or final-response tool—without planning, self-talk or protocol narration.`
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
- Decompose the complete requested output into propositions and assign each one to user dialogue, current attachments, current knowledge-base evidence or current web evidence. Never classify from isolated words.
- Set evidence_need="none" only when every requested proposition is grounded in user-authored dialogue or a current attachment. If any requested proposition needs document/domain evidence, set the matching source even when the primary intent is conversation_state.
- historical_assistant_output has no factual authority. Carrying forward, consolidating, translating, verifying or citing an external claim found only there requires current evidence; prior handles have expired.
- Source availability does not create evidence need. A dialogue-only transformation stays none even with a selected knowledge base. A negative, quoted or hypothetical operation does not suppress evidence required by a separate domain claim.
- Analysis or explanation is dialogue-grounded when every premise and requested conclusion is a transformation of user-authored facts. The business topic of that reasoning does not create external evidence need unless the user asks for an outside rule, standard, verification, or citation.
- evidence_query is the complete source-facing question for all externally grounded propositions and contains no dialogue bookkeeping or formatting. evidence_queries semantically decomposes it into one self-contained query per distinct source subject or unrelated claim group; use one entry for a single subject. Preserve identity-bearing object names, versions, identifiers, requested attributes and disambiguating qualifiers in every entry; never broaden them into a related topic. Leave both empty only for evidence_need="none".
- Preserve the current task, entities, values, chronology, modality, language and output scope. Unknown, pending, proposed, questioned, hypothetical, negated and completed are distinct. not-assert(P) never becomes assert(not-P), not-yet-P, rejected-P or incomplete-P. Never manufacture an event or revive an expired topic.`
	if strings.TrimSpace(prompt) == "" {
		return contract
	}
	return strings.TrimSpace(prompt) + "\n\n" + contract
}

// AppendCurrentTurnDirective keeps the original task once. Shared generation
// policy belongs in the system message, not duplicated user-message envelopes.
func AppendCurrentTurnDirective(content, originalQuery string) string { return content }

func TerminalGenerationDirective() string {
	return "Write one complete answer for the exact current task. Honor the requested language, count, form and scope; if an exact number of sentences, lines, items or sections is specified, count visible units and emit exactly that number. Keep planning, tool intention and protocol narration outside the visible body. Resolve omitted references to the most recent compatible user-authored object, not an older topic or retrieved subject without an explicit return. Apply a closed-source three-valued calculus: assert(P) permits P; assert(not-P) permits not-P; constrain(output, P) permits neither polarity. `not assert(P)` is not `assert(not-P)` and cannot become not-yet-P, rejected-P or incomplete-P. If neither polarity is sourced, preserve only the user's unresolved class when required. Require a source for the same object, field, relation, value and modality; shared vocabulary does not merge subjects. Do not strengthen evidence into a prerequisite, exclusivity, guarantee or causal relation. Use only requested categories and do not complete absent fields. Preserve chronology; current/latest selects the newest active user value for the same field, never runtime metadata. In a rewrite, draft, summary or handoff, use only sourced propositions plus neutral connectors that create no action, time, actor, outcome or commitment. Boundaries prove permission scope, not operation outcomes. Use only matching request-local citation handles; for item-by-item citations put one beside every supported item. Do not say that you will search or answer later: call a needed tool now, or answer now. Then output the private text delimiter " + TerminalAnswerOpen + " once, immediately followed by only the user-visible answer. It is transport text, not a tool; no final-answer or final-response tool exists."
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
The entries below are a bounded chronological provenance view of older user statements. Read omitted sources with read_conversation using their source_id; messages outside this window can be paged through with that tool. They are the factual authority for dialogue state; assistant history is context only. Resolve explicit updates by object and field, preserve each statement's modality, and never fill a missing value or operation outcome from a schema, role, example, question or absence of evidence.
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
