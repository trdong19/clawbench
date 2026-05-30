package ai

// piBackend is the CLIBackend instance for Pi CLI.
var piBackend = &CLIBackend{
	name:           "pi",
	defaultCommand: "pi",
	buildArgs:      buildPiStreamArgs,
	newParser:      func() LineParser { return &PiStreamParser{} },
	filterLine:     nil,
	preStart:       nil,
}

// buildPiStreamArgs constructs the CLI arguments for Pi streaming.
//
// Command: pi -p --mode json [flags] "prompt"
//
// Supported flags:
//
//	--session <id>              Resume a specific session
//	--continue                  Continue the most recent session
//	--no-session                Start a new session (no persistence)
//	--no-context-files          Skip AGENTS.md / CLAUDE.md discovery
//	--append-system-prompt <text> Append to Pi's built-in system prompt
//	--model <model>             Override model
//
// Working directory is set via cmd.Dir (CLIBackend sets cmd.Dir = req.WorkDir),
// not via a CLI flag — Pi does not have a --add-dir option.
func buildPiStreamArgs(req ChatRequest) []string {
	args := []string{"-p", "--mode", "json"}

	// Session management
	if req.Resume && req.SessionID != "" { //nolint:gocritic // ifElseChain: switch would be less readable for sequential condition checks
		// Resume a specific session by its Pi-assigned ID (captured via
		// external_session_id). This allows conversation continuity.
		args = append(args, "--session", req.SessionID)
	} else if req.Resume {
		// Resume without a known session ID — continue the most recent session.
		args = append(args, "--continue")
	} else if req.ScheduledExecution {
		// Scheduled tasks are independent executions — no need to persist sessions.
		args = append(args, "--no-session")
	} else { //nolint:staticcheck // SA9003: empty branch is intentional — no session flag for new interactive session
		// New interactive session: don't pass any session flag. Pi will
		// create a persistent session and emit its ID in a "session" event,
		// which we capture as external_session_id for future resumption.
		// Using --no-session here would make the session ephemeral, so the
		// captured ID would be unusable for --session resume later.
	}

	// Skip AGENTS.md / CLAUDE.md discovery — ClawBench injects its own rules
	args = append(args, "--no-context-files")

	// System prompt — use --append-system-prompt to preserve Pi's built-in prompt
	if req.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	}

	// Model override
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}

	// Thinking effort level (e.g., --thinking high)
	if req.ThinkingEffort != "" {
		args = append(args, "--thinking", req.ThinkingEffort)
	}

	// Prompt is the last positional argument
	args = append(args, req.Prompt)

	return args
}
