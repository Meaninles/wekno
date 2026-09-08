# Built-in agent policy

This module owns tenant-scoped policy for built-in agents only:

- `custom_builtin_agent_model_policies` is the source of truth for the main,
  rerank, VLM and ASR model bindings.
- `custom_builtin_agent_chat_visibility` controls whether a built-in agent is
  shown in that tenant's conversation picker.

Model bindings are overlaid on every read and re-applied in the runtime hook,
so changing an administrator setting is visible to all users and API replicas
without a process cache or a browser refresh. The update mutator accepts model
changes only from tenant administrators/system administrators; all other
built-in configuration stays in the native agent JSON pipeline. Custom agents
never enter this module.

The initial main-model policy uses Qwen for Knowledge Q&A and DeepSeek for all
other built-ins. Missing policy rows are initialized lazily and are also safe
to create during concurrent first reads. Conversation defaults show only
Knowledge Q&A, Document Processing and General Agent; an administrator can
change visibility from the built-in card switch.
