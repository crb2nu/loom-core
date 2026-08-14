Widen the pinned Claude Code hook-event fallback baseline from 18 to the 31
events documented at code.claude.com/docs/en/hooks as of 2026-08-07
(`pkg/generator/claude_hook_events.go`). `validateClaudeHookEvents` strips any
event not in the accepted set, and the baseline is what governs when the
installed CLI cannot be probed (e.g. CI), so hooks configured on any of the 13
newer events (`UserPromptExpansion`, `PermissionDenied`, `PostToolBatch`,
`MessageDisplay`, `TaskCreated`, `StopFailure`, `InstructionsLoaded`,
`CwdChanged`, `DirectoryAdded`, `FileChanged`, `PostCompact`, `Elicitation`,
`ElicitationResult`) were silently dropped on that path. The strip-unknown
validation itself is unchanged — one unknown event name silently disables ALL
Claude Code hooks — and machines with an installed binary still use the
probed enum. The Gemini baseline was verified against the current upstream
hooks reference (11 events, `PreCompress` spelling) and needed no change.
