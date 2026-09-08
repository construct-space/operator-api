// Construct cloud operator — a light, standalone agent harness.
//
// It is NOT the desktop brain. It carries no IDE, no code/file/browser
// tools, and holds no long-lived user credential. It does two things, both
// as the user's stand-in when their desktop is offline:
//
//  1. claim→run→report against Conductor's service path (scheduled
//     automations the desktop missed); and
//  2. answer a live question on POST /internal/ask (source-api routes a
//     phone "ask" here when the desktop operator is offline).
//
// Each run acts under a per-claim / per-request bearer token (a delegated
// "act-as-user" token for automations, the asker's own token for asks), so
// the cloud box never stores a user's credential. The agent loop, LLM
// client and tools are all in this module — nothing is imported from the
// desktop brain. See docs/architecture-overview.md (construct-app).
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// operatorID is the id this node claims under. It is deliberately NOT
	// "desktop", so Conductor's desktop-preference rule lets an online
	// desktop win the lease.
	operatorID = "cloud"
	// pollInterval is how long the claim loop idles when nothing was due.
	pollInterval = 20 * time.Second
	// defaultModel is the Construct-managed model id used when OPERATOR_MODEL
	// is unset. The provider gateway resolves it; override per deploy.
	defaultModel = "source-medium"

	scheduledTimeout = 3 * time.Minute
	askTimeout       = 90 * time.Second
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

func main() {
	conductorURL := strings.TrimRight(env("CONDUCTOR_URL", "https://conductor.lisaos.dev"), "/")
	secret := os.Getenv("INTERNAL_SHARED_SECRET")
	if secret == "" {
		fmt.Fprintln(os.Stderr, "[operator] INTERNAL_SHARED_SECRET is required")
		os.Exit(1)
	}
	port := env("PORT", "8090")
	// Empty => space actions are unavailable (web/LLM tools still work). Named
	// to match the brain + space-runtime convention.
	spaceRuntimeURL := os.Getenv("CONSTRUCT_SPACE_RUNTIME_URL")

	op := &operator{
		llm:          newLLMClient(),
		reg:          newRegistry(spaceRuntimeURL),
		model:        env("OPERATOR_MODEL", defaultModel),
		conductorURL: conductorURL,
		secret:       secret,
	}

	go op.serveHTTP(port)
	log.Printf("[operator] up; claiming from %s as %q (model=%s, space-runtime=%q)",
		conductorURL, operatorID, op.model, spaceRuntimeURL)
	op.claimLoop()
}

type operator struct {
	llm          *llmClient
	reg          *registry
	model        string
	conductorURL string
	secret       string
}

// ─── system prompts ──────────────────────────────────────────────────

const automationSystem = `You are a user's Construct operator, running one of their scheduled automations in the cloud because their desktop is offline. Carry out the instruction using the available tools: for a Space, call space_inspect first to discover its actions and skills, then space_run_action to run them; use web_search/web_fetch for current information. Be idempotent: the instruction has likely run before, so do not duplicate side effects already performed. When done, report concisely what you did or found.`

const askSystem = `You are the user's Construct assistant, answering on their behalf from their phone while their desktop is offline (you are running in the cloud). For their Spaces, use space_inspect to discover what a space can do, then space_run_action to act; use web_search/web_fetch to look things up. Answer the request directly and concisely; take actions only when the user clearly asks for them.`

// ─── claim loop (scheduled automations) ──────────────────────────────

func (o *operator) claimLoop() {
	for {
		if !o.claimOnce() {
			time.Sleep(pollInterval)
		}
	}
}

// claimOnce claims one due rule (service mode), runs it under the delegated
// owner token, and reports. Returns whether a rule actually ran.
func (o *operator) claimOnce() (ran bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[operator] recovered in claim: %v", r)
		}
	}()
	a, token := o.claim()
	if a == nil || token == "" {
		return false
	}
	log.Printf("[operator] claimed %q (%s)", a.Instruction, a.ID)
	ctx, cancel := context.WithTimeout(context.Background(), scheduledTimeout)
	defer cancel()
	res := o.runScheduled(ctx, token, *a)
	log.Printf("[operator] %s -> %s", a.ID, truncate(res, 200))
	o.report(token, a.ID, res)
	return true
}

func (o *operator) runScheduled(ctx context.Context, token string, a automation) string {
	prompt := "Scheduled automation instruction:\n" + a.Instruction
	if a.LastRunAt > 0 {
		prompt += fmt.Sprintf("\n\n(Last run: %s — avoid repeating side effects already done.)",
			time.Unix(a.LastRunAt, 0).UTC().Format(time.RFC3339))
	}
	res, err := runAgent(ctx, o.llm, o.model, automationSystem, prompt, token, o.reg)
	if err != nil {
		return "error: " + err.Error()
	}
	return res
}

// ─── HTTP server (/health, /internal/ask) ────────────────────────────

func (o *operator) serveHTTP(port string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"service":"operator"}`))
	})
	mux.HandleFunc("POST /internal/ask", o.handleAsk)
	addr := ":" + port
	log.Printf("[operator] http on %s (/health, POST /internal/ask)", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("[operator] http: %v", err)
	}
}

// handleAsk answers a relayed assistant question. source-api authenticates
// with the internal secret and passes the asking user's own bearer token, so
// the run acts as that user. Synchronous: returns the answer as JSON; source
// publishes it back into the device-bus hub keyed by request_id.
func (o *operator) handleAsk(w http.ResponseWriter, r *http.Request) {
	if !secretEqual(r.Header.Get("X-Internal-Secret"), o.secret) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var in struct {
		Token     string `json:"token"`
		Text      string `json:"text"`
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Token == "" || strings.TrimSpace(in.Text) == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	log.Printf("[operator] ask (req %s)", in.RequestID)
	ctx, cancel := context.WithTimeout(r.Context(), askTimeout)
	defer cancel()
	res, err := runAgent(ctx, o.llm, o.model, askSystem, in.Text, in.Token, o.reg)
	if err != nil {
		res = "Your desktop is offline and the cloud assistant couldn't finish this: " + err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"content": res, "request_id": in.RequestID})
}

// ─── Conductor client ────────────────────────────────────────────────

// automation mirrors the fields the operator needs from Conductor's claim
// response (api/conductor/store.go Automation).
type automation struct {
	ID          string `json:"id"`
	UserID      string `json:"user_id"`
	Instruction string `json:"instruction"`
	LastRunAt   int64  `json:"last_run_at"`
}

// claim hits Conductor's service-mode claim path with the internal secret and
// returns the claimed rule plus a delegated owner token (Conductor mints it
// via accounts). Sends both executor_id and operator_id so it works against
// the current schema and the pending operator_id rename.
func (o *operator) claim() (*automation, string) {
	body, _ := json.Marshal(map[string]any{
		"executor_id": operatorID,
		"operator_id": operatorID,
		"lease_secs":  180,
	})
	req, err := http.NewRequest("POST", o.conductorURL+"/api/claim", strings.NewReader(string(body)))
	if err != nil {
		return nil, ""
	}
	req.Header.Set("X-Internal-Secret", o.secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("[operator] claim error: %v", err)
		return nil, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[operator] claim http %d", resp.StatusCode)
		return nil, ""
	}
	var out struct {
		Automation *automation `json:"automation"`
		Token      string      `json:"token"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil, ""
	}
	return out.Automation, out.Token
}

// report posts the run result with the delegated token as bearer — it resolves
// to the rule's owner via accounts /me, so Conductor's user-authed report path
// accepts it.
func (o *operator) report(token, id, result string) {
	body, _ := json.Marshal(map[string]any{
		"id":          id,
		"executor_id": operatorID,
		"operator_id": operatorID,
		"result":      result,
	})
	req, err := http.NewRequest("POST", o.conductorURL+"/api/report", strings.NewReader(string(body)))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("[operator] report error: %v", err)
		return
	}
	_ = resp.Body.Close()
}

// ─── helpers ─────────────────────────────────────────────────────────

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func secretEqual(got, want string) bool {
	if want == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
