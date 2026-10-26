// artex-subscription-gateway exposes a deliberately small OpenAI Chat
// Completions facade backed by a locally authenticated Codex CLI. It never
// receives or stores an OpenAI API key: Codex obtains its credentials from its
// normal ChatGPT subscription login.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
	Stream bool `json:"stream"`
}

type codexEvent struct {
	Type string `json:"type"`
	Item struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Content string `json:"content"`
	} `json:"item"`
}

func main() {
	addr := flag.String("addr", ":8789", "listen address")
	codexBin := flag.String("codex", "codex", "path to the Codex CLI")
	workdir := flag.String("workdir", "/workspace", "working directory supplied to Codex")
	timeout := flag.Duration("timeout", 20*time.Minute, "maximum duration of one Codex turn")
	flag.Parse()

	g := gateway{codex: *codexBin, workdir: *workdir, timeout: *timeout}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", g.health)
	mux.HandleFunc("POST /v1/chat/completions", g.chat)
	log.Printf("subscription gateway listening on %s (Codex working directory: %s)", *addr, *workdir)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

type gateway struct {
	codex, workdir string
	timeout        time.Duration
}

func (g gateway) health(w http.ResponseWriter, _ *http.Request) {
	if _, err := exec.LookPath(g.codex); err != nil {
		http.Error(w, "codex CLI is not installed", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (g gateway) chat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Stream {
		writeError(w, http.StatusBadRequest, "the Codex subscription gateway supports stream:false only")
		return
	}
	prompt := messagesPrompt(req.Messages)
	if prompt == "" {
		writeError(w, http.StatusBadRequest, "a message is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), g.timeout)
	defer cancel()
	answer, err := g.run(ctx, req.Model, prompt)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":      fmt.Sprintf("chatcmpl-codex-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}},
	})
}

func (g gateway) run(ctx context.Context, model, prompt string) (string, error) {
	args := []string{"exec", "--json", "--skip-git-repo-check", "--sandbox", "workspace-write", "-C", g.workdir}
	if model != "" && model != "codex" {
		args = append(args, "--model", model)
	}
	args = append(args, prompt)
	cmd := exec.CommandContext(ctx, g.codex, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil { return "", err }
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil { return "", fmt.Errorf("start Codex: %w", err) }
	var answer strings.Builder
	s := bufio.NewScanner(stdout)
	buf := make([]byte, 0, 64*1024)
	s.Buffer(buf, 4<<20)
	for s.Scan() {
		var event codexEvent
		if json.Unmarshal(s.Bytes(), &event) != nil { continue }
		if event.Type == "item.completed" && event.Item.Type == "agent_message" {
			if event.Item.Text != "" { answer.WriteString(event.Item.Text) } else { answer.WriteString(event.Item.Content) }
		}
	}
	if err := s.Err(); err != nil { return "", err }
	if err := cmd.Wait(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) { return "", fmt.Errorf("Codex exceeded the %s timeout", g.timeout) }
		return "", fmt.Errorf("Codex failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if answer.Len() == 0 { return "", errors.New("Codex returned no final message") }
	return answer.String(), nil
}

func messagesPrompt(messages []struct { Role string `json:"role"`; Content any `json:"content"` }) string {
	var out strings.Builder
	for _, m := range messages {
		content := textContent(m.Content)
		if content == "" { continue }
		fmt.Fprintf(&out, "%s:\n%s\n\n", m.Role, content)
	}
	return strings.TrimSpace(out.String())
}

func textContent(v any) string {
	switch x := v.(type) {
	case string: return x
	case []any:
		var out strings.Builder
		for _, item := range x {
			if part, ok := item.(map[string]any); ok {
				if text, ok := part["text"].(string); ok { out.WriteString(text) }
			}
		}
		return out.String()
	default: return ""
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": "subscription_gateway_error"}})
}
