package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/erstaples/comfyform/comfy"
	"github.com/erstaples/comfyform/graph"
	"github.com/erstaples/comfyform/schema"
)

const (
	ProjectFile = "comfy.json"
	LockFile    = "comfy.lock.json"
	StateDir    = ".comfy"
)

// Project is a directory holding a workflow program, stack files and state.
type Project struct {
	Dir    string
	Name   string `json:"name"`
	Server string `json:"server"`
}

// StackFile is comfy.<stack>.json.
type StackFile struct {
	Server string       `json:"server,omitempty"`
	Config comfy.Config `json:"config"`
}

func LoadProject(dir string) (*Project, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	p := &Project{Dir: abs, Name: filepath.Base(abs)}
	b, err := os.ReadFile(filepath.Join(abs, ProjectFile))
	if err == nil {
		if err := json.Unmarshal(b, p); err != nil {
			return nil, fmt.Errorf("%s: %w", ProjectFile, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	p.Dir = abs
	return p, nil
}

func (p *Project) stackPath(stack string) string {
	return filepath.Join(p.Dir, "comfy."+stack+".json")
}

func (p *Project) Stack(stack string) (*StackFile, error) {
	sf := &StackFile{Config: comfy.Config{}}
	b, err := os.ReadFile(p.stackPath(stack))
	if errors.Is(err, os.ErrNotExist) {
		if stack != "default" {
			return nil, fmt.Errorf("no stack %q (expected %s)", stack, filepath.Base(p.stackPath(stack)))
		}
		return sf, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, sf); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(p.stackPath(stack)), err)
	}
	if sf.Config == nil {
		sf.Config = comfy.Config{}
	}
	return sf, nil
}

// Stacks lists stack names from comfy.<stack>.json files.
func (p *Project) Stacks() []string {
	matches, _ := filepath.Glob(filepath.Join(p.Dir, "comfy.*.json"))
	var out []string
	for _, m := range matches {
		n := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "comfy."), ".json")
		if n != "lock" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		out = []string{"default"}
	}
	return out
}

// ServerFor resolves the server: flag > stack file > comfy.json > $COMFY_SERVER > localhost.
func (p *Project) ServerFor(flag string, sf *StackFile) string {
	for _, s := range []string{flag, sf.Server, p.Server, os.Getenv("COMFY_SERVER")} {
		if s != "" {
			return s
		}
	}
	return "http://127.0.0.1:8188"
}

// Compile runs the workflow program for a stack and returns its prompt.
func (p *Project) Compile(ctx context.Context, stack string, sf *StackFile) (graph.Prompt, error) {
	tmp, err := os.CreateTemp("", "comfy-emit-*.json")
	if err != nil {
		return nil, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	cfg, _ := json.Marshal(sf.Config)
	cmd := exec.CommandContext(ctx, "go", "run", ".")
	cmd.Dir = p.Dir
	cmd.Env = append(os.Environ(), comfy.EnvEmit+"="+tmp.Name(), comfy.EnvStack+"="+stack, comfy.EnvConfig+"="+string(cfg))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("compiling stack %q failed:\n%s", stack, strings.TrimSpace(stderr.String()))
	}
	b, err := os.ReadFile(tmp.Name())
	if err != nil {
		return nil, err
	}
	var em comfy.Emitted
	if err := json.Unmarshal(b, &em); err != nil {
		return nil, fmt.Errorf("bad program output: %w", err)
	}
	return em.Prompt, nil
}

// --- schema loading ---

func (p *Project) schemaCache() string { return filepath.Join(p.Dir, StateDir, "object_info.json") }

// LoadSchema fetches /object_info (caching it) and falls back to the cache
// when the server is unreachable. live reports whether it came from the server.
func (p *Project) LoadSchema(ctx context.Context, c *Client) (s *schema.Schema, live bool, err error) {
	if c != nil {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		raw, ferr := c.ObjectInfo(cctx)
		if ferr == nil {
			s, err = schema.Parse(raw)
			if err != nil {
				return nil, false, err
			}
			_ = os.MkdirAll(filepath.Dir(p.schemaCache()), 0o755)
			_ = os.WriteFile(p.schemaCache(), raw, 0o644)
			return s, true, nil
		}
		err = ferr
	}
	raw, cerr := os.ReadFile(p.schemaCache())
	if cerr != nil {
		if err == nil {
			err = cerr
		}
		return nil, false, fmt.Errorf("no schema: server unreachable (%v) and no cache at %s", err, p.schemaCache())
	}
	s, perr := schema.Parse(raw)
	return s, false, perr
}

// --- state ---

type OutputFile struct {
	Path string `json:"path"`
	Hash string `json:"node_hash"`
}

type State struct {
	Stack     string                  `json:"stack"`
	AppliedAt time.Time               `json:"applied_at"`
	Server    string                  `json:"server"`
	PromptID  string                  `json:"prompt_id"`
	Prompt    graph.Prompt            `json:"prompt"`
	Hashes    map[string]string       `json:"hashes"`
	Outputs   map[string][]OutputFile `json:"outputs"`
}

func (p *Project) statePath(stack string) string {
	return filepath.Join(p.Dir, StateDir, "state", stack+".json")
}

func (p *Project) LoadState(stack string) (*State, error) {
	b, err := os.ReadFile(p.statePath(stack))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	return &s, json.Unmarshal(b, &s)
}

func (p *Project) SaveState(s *State) error {
	return writeJSON(p.statePath(s.Stack), s)
}

// OutputsCurrent reports whether every output node already has files on disk
// produced by an identical upstream graph.
func (s *State) OutputsCurrent(dir string, hashes map[string]string, outputNodes []string) bool {
	if s == nil || len(outputNodes) == 0 {
		return false
	}
	for _, id := range outputNodes {
		files := s.Outputs[id]
		if len(files) == 0 {
			return false
		}
		for _, f := range files {
			if f.Hash != hashes[id] {
				return false
			}
			if _, err := os.Stat(filepath.Join(dir, f.Path)); err != nil {
				return false
			}
		}
	}
	return true
}

// --- lockfile ---

type LockedClass struct {
	Module string `json:"module"`
	Schema string `json:"schema"`
}

type Lock struct {
	ComfyUIVersion string                 `json:"comfyui_version,omitempty"`
	Classes        map[string]LockedClass `json:"classes"`
	// Files maps "Class.input" to the file values the workflows reference.
	Files map[string][]string `json:"files"`
	// Packs maps each custom_nodes directory a locked class comes from to
	// the version installed at lock time.
	Packs map[string]LockedPack `json:"packs,omitempty"`
}

type LockedPack struct {
	Source  string `json:"source"`
	Content string `json:"content,omitempty"`
}

func (p *Project) LoadLock() (*Lock, error) {
	b, err := os.ReadFile(filepath.Join(p.Dir, LockFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var l Lock
	return &l, json.Unmarshal(b, &l)
}

func (p *Project) SaveLock(l *Lock) error { return writeJSON(filepath.Join(p.Dir, LockFile), l) }

// BuildLock records the classes, files and custom node packs the given
// prompts depend on. packs may be nil; every pack then locks as unpinned.
func BuildLock(prompts []graph.Prompt, s *schema.Schema, version string, packs *ServerPacks) *Lock {
	l := &Lock{ComfyUIVersion: version, Classes: map[string]LockedClass{}, Files: map[string][]string{}, Packs: map[string]LockedPack{}}
	seen := map[string]map[string]bool{}
	for _, pr := range prompts {
		for _, n := range pr {
			c := s.Classes[n.Class]
			if c == nil {
				continue
			}
			l.Classes[n.Class] = LockedClass{Module: c.Module, Schema: c.Hash()}
			if dir := PackDir(c.Module); dir != "" {
				lp := LockedPack{Source: Unpinned}
				if packs != nil {
					if pi, ok := packs.Packs[dir]; ok {
						lp = LockedPack{Source: pi.Source(), Content: pi.Content}
					}
				}
				l.Packs[dir] = lp
			}
			for _, in := range c.Inputs {
				if !in.IsFileList() {
					continue
				}
				v, ok := n.Inputs[in.Name].(string)
				if !ok || v == "" {
					continue
				}
				key := n.Class + "." + in.Name
				if seen[key] == nil {
					seen[key] = map[string]bool{}
				}
				if !seen[key][v] {
					seen[key][v] = true
					l.Files[key] = append(l.Files[key], v)
				}
			}
		}
	}
	for k := range l.Files {
		sort.Strings(l.Files[k])
	}
	return l
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
