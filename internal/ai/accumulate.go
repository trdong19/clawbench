// Package ai provides AI backend abstractions for streaming chat with CLI tools.
package ai

import (
	"encoding/json"

	"clawbench/internal/model"
)

// AccumulateBlock processes a single StreamEvent and updates the blocks slice.
// Both text and thinking events are coalesced into the most recent block of
// the same type; tool_use events are deduplicated by ID.
//
// When AI models (e.g. GLM-5.1) interleave thinking_delta and text_delta events,
// the last block may not be the same type as the incoming event. Instead of only
// checking the last block, we search backward for the most recent block of the
// same type and merge into it. However, tool_use blocks act as natural boundaries —
// text/thinking after a tool_use should not be merged with text/thinking before it.
// This prevents a single thinking or text block from being fragmented into many
// tiny blocks when events alternate, while preserving the semantic separation
// around tool calls.
func AccumulateBlock(blocks *[]model.ContentBlock, event StreamEvent) { //nolint:gocyclo,gocognit // 复杂的 stream 解析逻辑
	// findLastBlockOfType searches backward for the most recent block of the
	// given type, but stops at tool_use boundaries (they are natural separators).
	findLastBlockOfType := func(typ string) (int, bool) {
		for i := len(*blocks) - 1; i >= 0; i-- {
			if (*blocks)[i].Type == typ {
				return i, true
			}
			// tool_use blocks are natural boundaries — don't merge across them
			if (*blocks)[i].Type == "tool_use" { //nolint:goconst // JSON 字段名/协议字符串
				return -1, false
			}
		}
		return -1, false
	}

	switch event.Type {
	case "content": //nolint:goconst // JSON 字段名/协议字符串
		// Coalesce incremental content deltas into the most recent text block.
		if idx, found := findLastBlockOfType("text"); found {
			(*blocks)[idx].Text += event.Content
		} else {
			*blocks = append(*blocks, model.ContentBlock{Type: "text", Text: event.Content}) //nolint:goconst // JSON 字段名/协议字符串
		}
	case "thinking": //nolint:goconst // JSON 字段名/协议字符串
		// Coalesce incremental thinking deltas into the most recent thinking block.
		if idx, found := findLastBlockOfType("thinking"); found {
			(*blocks)[idx].Text += event.Content
		} else {
			*blocks = append(*blocks, model.ContentBlock{Type: "thinking", Text: event.Content})
		}
	case "tool_use":
		if event.Tool != nil {
			// Parse tool input JSON into map
			var input map[string]any
			if event.Tool.Input != "" {
				_ = json.Unmarshal([]byte(event.Tool.Input), &input)
			}
			if input == nil {
				input = make(map[string]any)
			}
			// Find existing block by tool ID and update, or append new
			found := false
			for i := len(*blocks) - 1; i >= 0; i-- {
				if (*blocks)[i].Type == "tool_use" && (*blocks)[i].ID == event.Tool.ID { //nolint:gocritic // nestingReduce: inverting would hurt readability
					(*blocks)[i].Input = input
					(*blocks)[i].Done = event.Tool.Done
					if event.Tool.Output != "" {
						(*blocks)[i].Output = event.Tool.Output
					}
					if event.Tool.Status != "" {
						(*blocks)[i].Status = event.Tool.Status
					}
					found = true
					break
				}
			}
			if !found {
				*blocks = append(*blocks, model.ContentBlock{
					Type:   "tool_use",
					Name:   event.Tool.Name,
					ID:     event.Tool.ID,
					Input:  input,
					Done:   event.Tool.Done,
					Output: event.Tool.Output,
					Status: event.Tool.Status,
				})
			}
		}
	case "tool_result": //nolint:goconst // JSON 字段名/协议字符串
		// tool_result events update the Output/Status of an existing tool_use block.
		// This handles backends (Gemini, Claude/Codebuddy stream_event) that send
		// tool results as a separate event after the tool_use event.
		if event.Tool != nil {
			for i := len(*blocks) - 1; i >= 0; i-- {
				if (*blocks)[i].Type == "tool_use" && (*blocks)[i].ID == event.Tool.ID {
					(*blocks)[i].Output = event.Tool.Output
					(*blocks)[i].Status = event.Tool.Status
					break
				}
			}
		}
	case "warning": //nolint:goconst // JSON 字段名/协议字符串
		*blocks = append(*blocks, model.ContentBlock{Type: "warning", Text: event.Content, Reason: event.Reason})
	case "error": //nolint:goconst // JSON 字段名/协议字符串
		*blocks = append(*blocks, model.ContentBlock{Type: "warning", Text: event.Error, Reason: event.Reason})
	}
}
