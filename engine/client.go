package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/erstaples/comfyform/graph"
)

// Client talks to a ComfyUI server's HTTP API.
type Client struct {
	Base string
	HTTP *http.Client
	ID   string
}

func NewClient(base string) *Client {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 60 * time.Second}, ID: "comfyform-" + hex.EncodeToString(b)}
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s", path, resp.Status, truncate(string(body), 300))
	}
	return body, nil
}

func (c *Client) ObjectInfo(ctx context.Context) ([]byte, error) { return c.get(ctx, "/object_info") }

// Version returns comfyui_version from /system_stats, or "" if unavailable.
func (c *Client) Version(ctx context.Context) string {
	b, err := c.get(ctx, "/system_stats")
	if err != nil {
		return ""
	}
	var s struct {
		System struct {
			Version string `json:"comfyui_version"`
		} `json:"system"`
	}
	_ = json.Unmarshal(b, &s)
	return s.System.Version
}

// Queue submits a prompt and returns its prompt_id.
func (c *Client) Queue(ctx context.Context, p graph.Prompt) (string, error) {
	body, _ := json.Marshal(map[string]any{"prompt": p, "client_id": c.ID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/prompt", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	var r struct {
		PromptID   string `json:"prompt_id"`
		Error      any    `json:"error"`
		NodeErrors map[string]struct {
			ClassType string `json:"class_type"`
			Errors    []struct {
				Message string `json:"message"`
				Details string `json:"details"`
			} `json:"errors"`
		} `json:"node_errors"`
	}
	_ = json.Unmarshal(rb, &r)
	if resp.StatusCode != http.StatusOK || r.PromptID == "" {
		var msgs []string
		for id, ne := range r.NodeErrors {
			for _, e := range ne.Errors {
				msgs = append(msgs, fmt.Sprintf("%s (%s): %s %s", id, ne.ClassType, e.Message, e.Details))
			}
		}
		if len(msgs) == 0 {
			msgs = append(msgs, truncate(string(rb), 500))
		}
		return "", fmt.Errorf("server rejected prompt:\n  %s", strings.Join(msgs, "\n  "))
	}
	return r.PromptID, nil
}

// FileRef is one output file reported in /history.
type FileRef struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

// Result is a finished prompt: output files per node id.
type Result struct {
	Outputs map[string][]FileRef
}

// Wait polls /history until the prompt completes or fails.
func (c *Client) Wait(ctx context.Context, promptID string, tick func(elapsed time.Duration)) (*Result, error) {
	start := time.Now()
	for {
		b, err := c.get(ctx, "/history/"+url.PathEscape(promptID))
		if err != nil {
			return nil, err
		}
		var h map[string]struct {
			Outputs map[string]map[string]json.RawMessage `json:"outputs"`
			Status  struct {
				StatusStr string `json:"status_str"`
				Completed bool   `json:"completed"`
				Messages  []any  `json:"messages"`
			} `json:"status"`
		}
		if err := json.Unmarshal(b, &h); err != nil {
			return nil, fmt.Errorf("history: %w", err)
		}
		if e, ok := h[promptID]; ok {
			if e.Status.StatusStr == "error" {
				return nil, fmt.Errorf("execution failed: %s", truncate(fmt.Sprint(e.Status.Messages), 800))
			}
			if e.Status.Completed || e.Status.StatusStr == "success" {
				res := &Result{Outputs: map[string][]FileRef{}}
				for node, kinds := range e.Outputs {
					for _, raw := range kinds { // images, gifs, audio, ...
						var files []FileRef
						if json.Unmarshal(raw, &files) == nil {
							for _, f := range files {
								if f.Filename != "" {
									res.Outputs[node] = append(res.Outputs[node], f)
								}
							}
						}
					}
				}
				return res, nil
			}
		}
		if tick != nil {
			tick(time.Since(start))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Download fetches an output file via /view.
func (c *Client) Download(ctx context.Context, f FileRef) ([]byte, error) {
	q := url.Values{"filename": {f.Filename}, "subfolder": {f.Subfolder}, "type": {f.Type}}
	return c.get(ctx, "/view?"+q.Encode())
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
