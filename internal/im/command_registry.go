package im

import "strings"

// CommandRegistry maps command names and complete-message aliases to their
// handlers.
type CommandRegistry struct {
	commands      map[string]Command
	exactCommands map[string]Command
	exactOnly     map[string]struct{}
}

// NewCommandRegistry returns an empty registry.
func NewCommandRegistry() *CommandRegistry {
	return &CommandRegistry{
		commands:      make(map[string]Command),
		exactCommands: make(map[string]Command),
		exactOnly:     make(map[string]struct{}),
	}
}

// Register adds cmd to the registry under its Name(). Panics on duplicate names
// to surface misconfiguration at startup rather than silently ignoring it.
func (r *CommandRegistry) Register(cmd Command) {
	key := strings.ToLower(cmd.Name())
	if _, exists := r.commands[key]; exists {
		panic("im: duplicate command registration: " + key)
	}
	r.commands[key] = cmd

	// Some commands are intentionally complete-message matches rather than
	// token-based commands. This prevents e.g. "/clear later" from being
	// interpreted as a request to clear the conversation.
	if matcher, ok := cmd.(interface{ ExactMatches() []string }); ok {
		matches := matcher.ExactMatches()
		if len(matches) == 0 {
			panic("im: command has no exact matches: " + key)
		}
		for _, match := range matches {
			exactKey := normalizeExactCommand(match)
			if exactKey == "" {
				panic("im: empty exact command match: " + key)
			}
			if _, exists := r.exactCommands[exactKey]; exists {
				panic("im: duplicate exact command registration: " + exactKey)
			}
			r.exactCommands[exactKey] = cmd
		}
		r.exactOnly[key] = struct{}{}
	}
}

// Parse checks whether content is a slash-command and, if so, returns the
// matching Command and the remaining tokens as args. Commands that expose
// ExactMatches are matched only when the complete message is one of those
// values and never receive token arguments.
//
// It returns (nil, nil, false) when:
//   - content does not start with "/"
//   - the first token after "/" has no registered handler
//
// Note: unrecognised slash-words are deliberately NOT matched here so that
// the caller can decide whether to treat them as unknown commands (show help)
// or pass them through to the QA pipeline (e.g. "/api/v2/users" paths).
// Use LooksLikeCommand to distinguish the two cases.
func (r *CommandRegistry) Parse(content string) (Command, []string, bool) {
	content = strings.TrimSpace(content)
	if cmd, ok := r.exactCommands[normalizeExactCommand(content)]; ok {
		return cmd, nil, true
	}
	if !strings.HasPrefix(content, "/") {
		return nil, nil, false
	}
	parts := strings.Fields(content[1:])
	if len(parts) == 0 {
		return nil, nil, false
	}
	name := strings.ToLower(parts[0])
	cmd, ok := r.commands[name]
	if !ok {
		return nil, nil, false
	}
	if _, exactOnly := r.exactOnly[name]; exactOnly {
		return nil, nil, false
	}
	return cmd, parts[1:], true
}

// IsRegistered returns true when content is an exact command match or starts
// with a registered token-based command name.
// It is cheaper than Parse because it does not allocate a result.
func (r *CommandRegistry) IsRegistered(content string) bool {
	content = strings.TrimSpace(content)
	if _, ok := r.exactCommands[normalizeExactCommand(content)]; ok {
		return true
	}
	if !strings.HasPrefix(content, "/") {
		return false
	}
	parts := strings.Fields(content[1:])
	if len(parts) == 0 {
		return false
	}
	name := strings.ToLower(parts[0])
	if _, exactOnly := r.exactOnly[name]; exactOnly {
		return false
	}
	_, ok := r.commands[name]
	return ok
}

func normalizeExactCommand(content string) string {
	return strings.ToLower(strings.TrimSpace(content))
}

// All returns every registered command.
func (r *CommandRegistry) All() []Command {
	cmds := make([]Command, 0, len(r.commands))
	for _, cmd := range r.commands {
		cmds = append(cmds, cmd)
	}
	return cmds
}

// LooksLikeCommand returns true when content appears to be a command attempt—
// it starts with "/" and the first token contains no further "/" separators.
//
// This distinguishes "/help" (command attempt) from "/api/v2/users" (URL path
// that should fall through to the QA pipeline).
func LooksLikeCommand(content string) bool {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "/") {
		return false
	}
	parts := strings.Fields(content[1:])
	if len(parts) == 0 {
		return false
	}
	return !strings.Contains(parts[0], "/")
}
