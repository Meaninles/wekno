package im

import "testing"

func TestCommandRegistryClearCommandsRequireExactMatch(t *testing.T) {
	registry := NewCommandRegistry()
	clear := newClearCommand()
	registry.Register(clear)

	tests := []struct {
		name      string
		content   string
		wantMatch bool
		wantArgs  int
	}{
		{name: "Chinese new alias", content: "新建对话", wantMatch: true},
		{name: "Chinese short new alias", content: "新对话", wantMatch: true},
		{name: "slash clear", content: "/clear", wantMatch: true},
		{name: "slash new", content: "/new", wantMatch: true},
		{name: "surrounding whitespace is trimmed", content: "  /new  ", wantMatch: true},
		{name: "clear with argument is not exact", content: "/clear later", wantMatch: false},
		{name: "new with argument is not exact", content: "/new later", wantMatch: false},
		{name: "Chinese new with suffix is not exact", content: "新建对话吧", wantMatch: false},
		{name: "Chinese short new with suffix is not exact", content: "新对话吧", wantMatch: false},
		{name: "embedded clear is not exact", content: "请执行 /clear", wantMatch: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, args, ok := registry.Parse(tt.content)
			if ok != tt.wantMatch {
				t.Fatalf("Parse(%q) matched=%v, want %v", tt.content, ok, tt.wantMatch)
			}
			if tt.wantMatch {
				if cmd != clear {
					t.Fatalf("Parse(%q) returned command %T, want ClearCommand", tt.content, cmd)
				}
				if len(args) != tt.wantArgs {
					t.Fatalf("Parse(%q) args=%v, want %d args", tt.content, args, tt.wantArgs)
				}
			}
			if registry.IsRegistered(tt.content) != tt.wantMatch {
				t.Fatalf("IsRegistered(%q)=%v, want %v", tt.content, registry.IsRegistered(tt.content), tt.wantMatch)
			}
		})
	}
}

func TestCommandRegistryKeepsTokenArgumentsForRegularCommands(t *testing.T) {
	registry := NewCommandRegistry()
	registry.Register(newHelpCommand(registry))

	cmd, args, ok := registry.Parse("/help command-name")
	if !ok {
		t.Fatal("Parse(/help command-name) did not match the regular command")
	}
	if cmd.Name() != "help" {
		t.Fatalf("Parse returned %q, want help", cmd.Name())
	}
	if len(args) != 1 || args[0] != "command-name" {
		t.Fatalf("Parse args=%v, want [command-name]", args)
	}
}
