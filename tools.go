// tools.go — the operator's tool surface and registry. Deliberately tiny:
//
//	space_run_action  — the operator's hands; run an action on an installed
//	                     Space, headless, via the space-runtime service.
//	web_search        — look something up (DuckDuckGo, no key).  [web.go]
//	web_fetch         — read a URL.                              [web.go]
//
// No read/write/edit/bash/git/browser — there is no user machine in the cloud,
// and code work belongs to the desktop brain. The per-run bearer token is
// passed explicitly to Execute (the cloud holds no long-lived credential).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Tool is the operator's tool contract — a thin echo of the brain's, but the
// run's bearer token is passed in rather than pinned in process-global state.
type Tool interface {
	Name() string
	Description() string
	Schema() map[string]any
	Execute(ctx context.Context, args json.RawMessage, token string) (string, error)
}

type registry struct {
	tools map[string]Tool
	order []string
}

func newRegistry(spaceRuntimeURL string) *registry {
	rt := strings.TrimRight(spaceRuntimeURL, "/")
	r := &registry{tools: map[string]Tool{}}
	r.add(SpaceInspect{runtimeURL: rt})
	r.add(SpaceRunAction{runtimeURL: rt})
	r.add(WebSearch{})
	r.add(WebFetch{})
	return r
}

func (r *registry) add(t Tool) {
	r.tools[t.Name()] = t
	r.order = append(r.order, t.Name())
}

// providerTools renders the registered tools as OpenAI function-tool specs.
func (r *registry) providerTools() []map[string]any {
	out := make([]map[string]any, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name(),
				"description": t.Description(),
				"parameters":  t.Schema(),
			},
		})
	}
	return out
}

// run dispatches one tool call, turning a missing tool or an error into
// model-readable text (so the loop can recover rather than abort).
func (r *registry) run(ctx context.Context, name string, args json.RawMessage, token string) string {
	t := r.tools[name]
	if t == nil {
		return "error: unknown tool " + name
	}
	out, err := t.Execute(ctx, args, token)
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}

// ─── space_inspect ───────────────────────────────────────────────────

// SpaceInspect fetches what a Space offers — its actions (with descriptions +
// params), its skills, and its agent persona — from space-runtime's
// GET /space/{name}. This is how the light operator "becomes" a space's agent:
// it discovers the space's capabilities, then drives them with space_run_action.
type SpaceInspect struct{ runtimeURL string }

func (SpaceInspect) Name() string { return "space_inspect" }

func (SpaceInspect) Description() string {
	return "Inspect an installed Space before acting on it: returns its actions (id, description, params), its skills (procedures), and its agent persona. Call this first when a task involves a space you haven't inspected, then use space_run_action to run the right action."
}

func (SpaceInspect) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"space":   map[string]any{"type": "string", "description": "Space id, e.g. \"mail\" or \"calendar\"."},
			"version": map[string]any{"type": "string", "description": "Optional published version; defaults to the space's latest."},
		},
		"required": []string{"space"},
	}
}

func (s SpaceInspect) Execute(ctx context.Context, raw json.RawMessage, _ string) (string, error) {
	if s.runtimeURL == "" {
		return "", fmt.Errorf("space inspection is unavailable here: CONSTRUCT_SPACE_RUNTIME_URL is not configured")
	}
	var in struct {
		Space   string `json:"space"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if strings.TrimSpace(in.Space) == "" {
		return "", fmt.Errorf("space is required")
	}
	path := s.runtimeURL + "/space/" + url.PathEscape(in.Space)
	if in.Version != "" {
		path += "/" + url.PathEscape(in.Version)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", path, nil)
	if err != nil {
		return "", err
	}
	resp, err := spaceRunClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var probe struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(out, &probe)
	if !probe.OK {
		if probe.Error == "" {
			probe.Error = fmt.Sprintf("http %d", resp.StatusCode)
		}
		return "", fmt.Errorf("space inspect failed: %s", probe.Error)
	}
	return string(out), nil
}

// ─── space_run_action ────────────────────────────────────────────────

// SpaceRunAction invokes a Space action headless via the space-runtime
// service (POST /run). Everything the operator can *do* on the user's behalf —
// read their mail, add a calendar entry, update a record — is a Space action.
type SpaceRunAction struct{ runtimeURL string }

func (SpaceRunAction) Name() string { return "space_run_action" }

func (SpaceRunAction) Description() string {
	return "Run an action exposed by one of the user's installed Spaces (e.g. space \"mail\" action \"list_unread\", or space \"calendar\" action \"next_event\"). Runs headless via the space-runtime. Provide the space id, the action id, and any args the action declares."
}

func (SpaceRunAction) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"space":   map[string]any{"type": "string", "description": "Space id, e.g. \"mail\" or \"calendar\"."},
			"action":  map[string]any{"type": "string", "description": "Action id to run, e.g. \"list_unread\"."},
			"args":    map[string]any{"type": "object", "description": "Action arguments object (per the action's own schema). Omit if none."},
			"version": map[string]any{"type": "string", "description": "Optional published bundle version; defaults to the space's latest."},
		},
		"required": []string{"space", "action"},
	}
}

var spaceRunClient = &http.Client{Timeout: 90 * time.Second}

func (s SpaceRunAction) Execute(ctx context.Context, raw json.RawMessage, token string) (string, error) {
	if s.runtimeURL == "" {
		return "", fmt.Errorf("space actions are unavailable here: CONSTRUCT_SPACE_RUNTIME_URL is not configured")
	}
	var in struct {
		Space   string          `json:"space"`
		Action  string          `json:"action"`
		Args    json.RawMessage `json:"args"`
		Version string          `json:"version"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if in.Space == "" || in.Action == "" {
		return "", fmt.Errorf("space and action are required")
	}

	payload := map[string]any{
		"spaceId": in.Space,
		"action":  in.Action,
		"params":  emptyObjIfNil(in.Args),
		"token":   token,
	}
	// Cloud callers pass {name, version} so space-runtime fetches + verifies
	// the published bundle rather than expecting a local checkout.
	if in.Version != "" {
		payload["name"] = in.Space
		payload["version"] = in.Version
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", s.runtimeURL+"/run", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := spaceRunClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var res struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if json.Unmarshal(out, &res) != nil {
		return "", fmt.Errorf("space-runtime: unparseable response (http %d)", resp.StatusCode)
	}
	if !res.OK {
		if res.Error == "" {
			res.Error = fmt.Sprintf("http %d", resp.StatusCode)
		}
		return "", fmt.Errorf("space action failed: %s", res.Error)
	}
	if len(res.Result) == 0 {
		return "(action completed, no result returned)", nil
	}
	return string(res.Result), nil
}

func emptyObjIfNil(r json.RawMessage) json.RawMessage {
	if len(r) == 0 {
		return json.RawMessage("{}")
	}
	return r
}
