// Мост к Codex: Сёма живёт в контейнере, а клиент Codex — на маке. Мост
// поднимается на маке и говорит по протоколу OpenAI, поэтому Сёме он виден
// как обычная модель, а внутри запускает `codex exec` подпроцессом.
//
// Зачем: суду над командами нужны РАЗНЫЕ модели (арбитр и судья), и Codex
// здесь — вторая голова, к тому же по подписке, без ключей API.
//
// Запуск на маке (из корня репозитория):
//
//	CODEX_BRIDGE_TOKEN=<длинная-строка> go run ./integrations/codex-bridge
//
// Слушает 0.0.0.0:8791 (порт меняется CODEX_BRIDGE_ADDR), требует
// заголовок Authorization: Bearer <токен> — без него отказ. Codex зовётся
// только на чтение (--sandbox read-only): мост не выполняет команд.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type chatReq struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

type chatResp struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		Prompt     int `json:"prompt_tokens"`
		Completion int `json:"completion_tokens"`
		Total      int `json:"total_tokens"`
	} `json:"usage"`
}

func main() {
	token := strings.TrimSpace(os.Getenv("CODEX_BRIDGE_TOKEN"))
	if token == "" {
		log.Fatal("нужен CODEX_BRIDGE_TOKEN: без него мост открыт всей сети")
	}
	addr := os.Getenv("CODEX_BRIDGE_ADDR")
	if addr == "" {
		addr = "0.0.0.0:8791"
	}
	if _, err := exec.LookPath("codex"); err != nil {
		log.Fatal("клиент codex не найден в PATH")
	}

	mux := http.NewServeMux()
	guard := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"error":"нужен токен моста"}`, http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		writeJSON(w, map[string]any{"object": "list", "data": []map[string]any{
			{"id": "codex", "object": "model", "owned_by": "openai"},
		}})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		var req chatReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"тело запроса не разобрано"}`, http.StatusBadRequest)
			return
		}
		var b strings.Builder
		for _, m := range req.Messages {
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			if m.Role == "system" {
				b.WriteString(m.Content + "\n\n")
				continue
			}
			b.WriteString(m.Content + "\n")
		}
		prompt := strings.TrimSpace(b.String())
		if prompt == "" {
			http.Error(w, `{"error":"пустой запрос"}`, http.StatusBadRequest)
			return
		}
		answer, err := runCodex(prompt, req.Model)
		if err != nil {
			log.Printf("codex: %v", err)
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadGateway)
			return
		}
		out := chatResp{ID: "codex-" + fmt.Sprint(time.Now().UnixNano()), Object: "chat.completion",
			Created: time.Now().Unix(), Model: firstNonEmpty(req.Model, "codex")}
		out.Choices = append(out.Choices, struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		}{})
		out.Choices[0].Message.Role = "assistant"
		out.Choices[0].Message.Content = answer
		out.Choices[0].FinishReason = "stop"
		// точных токенов клиент не отдаёт: пусть Сёма считает оценкой
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
	})

	log.Printf("мост к Codex слушает %s", addr)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// runCodex — один вызов клиента. Только чтение и без следов на диске:
// мост — вторая голова для суда, а не исполнитель.
func runCodex(prompt, model string) (string, error) {
	dir, err := os.MkdirTemp("", "codex-bridge")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	last := filepath.Join(dir, "answer.txt")

	args := []string{"exec", "--skip-git-repo-check", "--ephemeral", "--sandbox", "read-only",
		"--color", "never", "-o", last}
	if model != "" && model != "codex" {
		args = append(args, "--model", model)
	}
	args = append(args, prompt)

	cmd := exec.Command("codex", args...)
	cmd.Stdin = strings.NewReader("") // иначе клиент ждёт ввода и висит
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	cmd.Stdout = &strings.Builder{}
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			msg := strings.TrimSpace(errBuf.String())
			if msg == "" {
				msg = err.Error()
			}
			return "", fmt.Errorf("codex: %s", clip(msg, 300))
		}
	case <-time.After(10 * time.Minute):
		_ = cmd.Process.Kill()
		return "", fmt.Errorf("codex не ответил за 10 минут")
	}
	raw, err := os.ReadFile(last)
	if err != nil {
		return "", fmt.Errorf("ответ не прочитан: %w", err)
	}
	answer := strings.TrimSpace(string(raw))
	if answer == "" {
		return "", fmt.Errorf("codex вернул пустой ответ")
	}
	return answer, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
