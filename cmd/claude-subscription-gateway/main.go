// artex-claude-subscription-gateway exposes a small OpenAI Chat Completions
// facade backed by a locally authenticated Claude Code CLI. It never accepts,
// stores, or forwards an Anthropic API key.
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

type message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Stream   bool      `json:"stream"`
}

type claudeEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Result  string `json:"result"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

type gateway struct {
	claude, workdir string
	timeout         time.Duration
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8790", "listen address")
	claudeBin := flag.String("claude", "claude", "path to the Claude Code CLI")
	workdir := flag.String("workdir", ".", "working directory supplied to Claude")
	timeout := flag.Duration("timeout", 20*time.Minute, "maximum duration of one Claude turn")
	flag.Parse()

	g := gateway{claude: *claudeBin, workdir: *workdir, timeout: *timeout}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", g.health)
	mux.HandleFunc("POST /v1/chat/completions", g.chat)
	log.Printf("Claude subscription gateway listening on %s (working directory: %s)", *addr, *workdir)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func (g gateway) health(w http.ResponseWriter, _ *http.Request) {
	if _, err := exec.LookPath(g.claude); err != nil {
		http.Error(w, "Claude Code CLI is not installed", http.StatusServiceUnavailable)
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
		writeError(w, http.StatusBadRequest, "the Claude subscription gateway supports stream:false only")
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
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":      fmt.Sprintf("chatcmpl-claude-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}},
	})
}

func (g gateway) run(ctx context.Context, model, prompt string) (string, error) {
	args := []string{"-p", "--verbose", "--output-format", "stream-json", "--no-session-persistence", "--permission-mode", "auto"}
	if model != "" && model != "claude" {
		args = append(args, "--model", model)
	}
	args = append(args, "--", prompt)
	cmd := exec.CommandContext(ctx, g.claude, args...)
	cmd.Dir = g.workdir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start Claude: %w", err)
	}
	var answer strings.Builder
	s := bufio.NewScanner(stdout)
	s.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for s.Scan() {
		var event claudeEvent
		if json.Unmarshal(s.Bytes(), &event) != nil {
			continue
		}
		if event.Type == "assistant" {
			for _, part := range event.Message.Content {
				if part.Type == "text" {
					answer.WriteString(part.Text)
				}
			}
		}
		if event.Type == "result" && event.Subtype == "success" && answer.Len() == 0 {
			answer.WriteString(event.Result)
		}
	}
	if err := s.Err(); err != nil {
		return "", err
	}
	if err := cmd.Wait(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("Claude exceeded the %s timeout", g.timeout)
		}
		return "", fmt.Errorf("Claude failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if answer.Len() == 0 {
		return "", errors.New("Claude returned no final message")
	}
	return answer.String(), nil
}

func messagesPrompt(messages []message) string {
	var out strings.Builder
	for _, m := range messages {
		if content := textContent(m.Content); content != "" {
			fmt.Fprintf(&out, "%s:\n%s\n\n", m.Role, content)
		}
	}
	return strings.TrimSpace(out.String())
}

func textContent(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var out strings.Builder
		for _, item := range x {
			if part, ok := item.(map[string]any); ok {
				if text, ok := part["text"].(string); ok {
					out.WriteString(text)
				}
			}
		}
		return out.String()
	default:
		return ""
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": "subscription_gateway_error"}})
}
