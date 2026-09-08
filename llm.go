// llm.go — the LLM client. One authenticated POST per turn to the Construct
// provider gateway's OpenAI-compatible /chat/completions, billed to the run's
// owner via their bearer token (the delegated automation token, or the asker's
// token for /internal/ask). Pattern borrowed from api/tv (openaiChat), extended
// to carry a full message history + function tools for the agent loop.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// chatMessage is one OpenAI-style message. Content is omitted on an
// assistant turn that is pure tool calls; ToolCallID is set on tool results.
type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolCallFunc `json:"function"`
}

type toolCallFunc struct {
	Name string `json:"name"`
	// Arguments is a JSON-encoded string (OpenAI tool-call shape), parsed by
	// the receiving tool.
	Arguments string `json:"arguments"`
}

// llmClient calls the Construct provider gateway. base is the chat root
// (…/api/construct); chat() appends /chat/completions.
type llmClient struct {
	base   string
	client *http.Client
}

func newLLMClient() *llmClient {
	gw := strings.TrimRight(env("GATEWAY_URL", "https://my.lisaos.dev"), "/")
	base := strings.TrimRight(env("OPERATOR_LLM_BASE", gw+"/api/construct"), "/")
	return &llmClient{base: base, client: &http.Client{Timeout: 120 * time.Second}}
}

// chat runs one completion. tools may be empty. token authenticates (and bills)
// the call as the run's owner. Returns the assistant message (content and/or
// tool_calls).
func (l *llmClient) chat(ctx context.Context, token, model string, messages []chatMessage, tools []map[string]any) (chatMessage, error) {
	payload := map[string]any{
		"model":      model,
		"messages":   messages,
		"max_tokens": 2048,
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", l.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return chatMessage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := l.client.Do(req)
	if err != nil {
		return chatMessage{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return chatMessage{}, fmt.Errorf("llm http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var cr struct {
		Choices []struct {
			Message chatMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &cr); err != nil {
		return chatMessage{}, fmt.Errorf("llm: bad response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return chatMessage{}, fmt.Errorf("llm: no choices in response")
	}
	return cr.Choices[0].Message, nil
}
