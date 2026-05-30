// Package summarize provides text summarization backends (AI, OpenAI, Anthropic) for TTS and task summaries.
package summarize

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"clawbench/internal/ai"
)

const (
	eventTypeContent = "content"
	eventTypeDone    = "done"
	eventTypeError   = "error"
	blockTypeText    = "text"
)

// AIBackendSummarizer implements Summarizer using an existing AI backend
// (claude, codebuddy, gemini, opencode, codex, qoder, vecli) via Backend.ExecuteStream().
// The full name is retained to avoid confusion with ai.Backend when both packages
// are imported in the same file.
type AIBackendSummarizer struct {
	backend ai.AIBackend
	// Model is the model ID override for the AI backend (empty = use backend default).
	Model string
	gs    Pipeline
}

// NewAIBackendSummarizer creates an AIBackendSummarizer for the given backend type.
func NewAIBackendSummarizer(backendType string) (*AIBackendSummarizer, error) {
	backend, err := ai.NewBackend(backendType)
	if err != nil {
		return nil, fmt.Errorf("failed to create AI backend for summarization: %w", err)
	}
	s := &AIBackendSummarizer{
		backend: backend,
	}
	s.gs = NewTTSPipeline(s.DoSummarizePass)
	return s, nil
}

// Summarize condenses text for voice output using an AI backend.
// It sends a single-turn request and collects content events from the stream.
func (s *AIBackendSummarizer) Summarize(ctx context.Context, text, language string) (string, error) {
	return s.gs.Summarize(ctx, text, language)
}

// DoSummarizePass performs a single summarization pass using an AI backend.
func (s *AIBackendSummarizer) DoSummarizePass(ctx context.Context, text, systemPrompt string, pass int) (string, error) {
	req := ai.ChatRequest{
		Prompt:       text,
		SessionID:    "", // single-turn, no session
		WorkDir:      "", // no workdir needed for summarization
		SystemPrompt: systemPrompt,
		Model:        s.Model, // use configured model or backend default
		Command:      "",      // use backend default command
		AgentID:      "",      // not associated with any agent
		Resume:       false,   // single-turn, no resume
	}

	ch, err := s.backend.ExecuteStream(ctx, req)
	if err != nil {
		return "", fmt.Errorf("AI backend %q (pass %d) failed to start: %w", s.backend.Name(), pass, err)
	}

	// Collect content events from the stream
	var buf strings.Builder
	for event := range ch {
		switch event.Type {
		case eventTypeContent:
			buf.WriteString(event.Content)
		case eventTypeDone:
			// Stream completed successfully
		case eventTypeError:
			return "", fmt.Errorf("AI backend %q (pass %d) error: %s", s.backend.Name(), pass, event.Error)
		}
	}

	result := strings.TrimSpace(buf.String())
	if result == "" {
		return "", fmt.Errorf("AI backend %q (pass %d) returned empty output", s.backend.Name(), pass)
	}

	slog.Info(
		"tts summarize pass completed",
		slog.Int("pass", pass),
		slog.String("backend", s.backend.Name()),
		slog.Int("result_len", len([]rune(result))),
	)

	return result, nil
}
