// agent.go — the harness. A minimal tool-calling loop: send the system +
// prompt, run any tool calls the model asks for, feed results back, repeat
// until the model answers without tools or we hit the iteration cap. No
// planning, no sub-agents, no memory — that heavier work stays in the desktop
// brain. This is just "task in → small loop → answer out".
package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// maxIterations bounds the tool loop so a confused model can't spin forever.
const maxIterations = 12

// runAgent drives one conversation to completion. token authenticates both the
// LLM call and every tool that needs it (space_run_action), so the whole run
// acts as a single owner.
func runAgent(ctx context.Context, llm *llmClient, model, system, prompt, token string, reg *registry) (string, error) {
	toolDefs := reg.providerTools()
	messages := []chatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: prompt},
	}
	var last string
	for i := 0; i < maxIterations; i++ {
		msg, err := llm.chat(ctx, token, model, messages, toolDefs)
		if err != nil {
			return "", err
		}
		if msg.Content != "" {
			last = msg.Content
		}
		// No tool calls → the model has answered.
		if len(msg.ToolCalls) == 0 {
			return msg.Content, nil
		}
		// Record the assistant's tool-call turn, then run each call and feed
		// the results back as tool messages.
		messages = append(messages, msg)
		for _, tc := range msg.ToolCalls {
			out := reg.run(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments), token)
			messages = append(messages, chatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    out,
			})
		}
	}
	if last != "" {
		return last + "\n\n(stopped: reached the tool-iteration limit.)", nil
	}
	return "", fmt.Errorf("reached the tool-iteration limit without an answer")
}
