// Package fakecomfy is a minimal stand-in for ComfyUI's HTTP API, for tests
// and offline demos. It validates class names, "executes" instantly, and
// returns a small PNG per output node.
package fakecomfy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"sync"
)

type Server struct {
	objectInfo []byte
	classes    map[string]struct {
		OutputNode bool `json:"output_node"`
	}

	mu      sync.Mutex
	n       int
	history map[string]map[string]any
	files   map[string][]byte
	Queued  []map[string]any // prompts received, for assertions
}

func New(objectInfo []byte) (*Server, error) {
	s := &Server{objectInfo: objectInfo, history: map[string]map[string]any{}, files: map[string][]byte{}}
	if err := json.Unmarshal(objectInfo, &s.classes); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/object_info":
		w.Header().Set("Content-Type", "application/json")
		w.Write(s.objectInfo)
	case r.URL.Path == "/system_stats":
		writeJSON(w, 200, map[string]any{"system": map[string]any{"comfyui_version": "0.3.99-fake"}})
	case r.URL.Path == "/prompt" && r.Method == http.MethodPost:
		s.prompt(w, r)
	case strings.HasPrefix(r.URL.Path, "/history/"):
		s.mu.Lock()
		id := strings.TrimPrefix(r.URL.Path, "/history/")
		h, ok := s.history[id]
		s.mu.Unlock()
		if !ok {
			writeJSON(w, 200, map[string]any{})
			return
		}
		writeJSON(w, 200, map[string]any{id: h})
	case r.URL.Path == "/view":
		s.mu.Lock()
		b, ok := s.files[r.URL.Query().Get("filename")]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) prompt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Prompt map[string]struct {
			Class  string         `json:"class_type"`
			Inputs map[string]any `json:"inputs"`
		} `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"message": err.Error()}, "node_errors": map[string]any{}})
		return
	}
	nodeErrors := map[string]any{}
	var outputs []string
	for id, n := range body.Prompt {
		c, ok := s.classes[n.Class]
		if !ok {
			nodeErrors[id] = map[string]any{"class_type": n.Class, "errors": []map[string]any{{"message": "Node class not found", "details": n.Class}}}
			continue
		}
		if c.OutputNode {
			outputs = append(outputs, id)
		}
	}
	if len(nodeErrors) > 0 || len(outputs) == 0 {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"type": "prompt_outputs_failed_validation", "message": "Prompt outputs failed validation"}, "node_errors": nodeErrors})
		return
	}

	raw, _ := json.Marshal(body.Prompt)
	s.mu.Lock()
	defer s.mu.Unlock()
	var asMap map[string]any
	_ = json.Unmarshal(raw, &asMap)
	s.Queued = append(s.Queued, asMap)
	s.n++
	id := fmt.Sprintf("fake-%04d", s.n)

	out := map[string]any{}
	for _, node := range outputs {
		prefix := "ComfyUI"
		if p, ok := body.Prompt[node].Inputs["filename_prefix"].(string); ok && p != "" {
			prefix = p
		}
		name := fmt.Sprintf("%s_%05d_.png", prefix, s.n)
		s.files[name] = swatch(raw, node)
		out[node] = map[string]any{"images": []map[string]any{{"filename": name, "subfolder": "", "type": "output"}}}
	}
	s.history[id] = map[string]any{
		"outputs": out,
		"status":  map[string]any{"status_str": "success", "completed": true, "messages": []any{}},
	}
	writeJSON(w, 200, map[string]any{"prompt_id": id, "number": s.n, "node_errors": map[string]any{}})
}

// swatch is a 64x64 image whose color depends on the prompt, so different
// graphs visibly produce different "renders".
func swatch(prompt []byte, node string) []byte {
	sum := sha256.Sum256(append(prompt, node...))
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{sum[0], sum[1] ^ uint8(x*2), sum[2] ^ uint8(y*2), 255})
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
