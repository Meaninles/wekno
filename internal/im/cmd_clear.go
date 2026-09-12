package im

import "context"

// ClearCommand implements the conversation-reset commands.
// It soft-deletes the current ChannelSession and clears the LLM context so
// the next message starts a completely fresh conversation.
type ClearCommand struct{}

func newClearCommand() *ClearCommand { return &ClearCommand{} }

func (c *ClearCommand) Name() string { return "clear" }
func (c *ClearCommand) Description() string {
	return "重新开始对话（支持：新建对话、新对话、/clear、/new）"
}

// ExactMatches keeps reset commands unambiguous: only these complete messages
// trigger a reset, while messages such as "/clear 说明" continue as normal
// user messages/unknown-command handling and cannot clear a session.
func (c *ClearCommand) ExactMatches() []string {
	return []string{"新建对话", "新对话", "/clear", "/new"}
}

func (c *ClearCommand) Execute(_ context.Context, _ *CommandContext, _ []string) (*CommandResult, error) {
	return &CommandResult{
		Content: "✅ 对话已清空，下次消息将开始全新会话。",
		Action:  ActionClear,
	}, nil
}
