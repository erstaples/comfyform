package engine

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// PackInfo describes one installed custom node pack (a directory under
// custom_nodes, or a single .py file there).
type PackInfo struct {
	Kind    string `json:"kind"` // git | registry | local
	Remote  string `json:"remote,omitempty"`
	Commit  string `json:"commit,omitempty"`
	ID      string `json:"id,omitempty"`
	Version string `json:"version,omitempty"`
	Content string `json:"content,omitempty"`
}

// Unpinned marks a pack the server couldn't identify.
const Unpinned = "unpinned"

// Source is the pack's pinned origin, as stored in the lockfile.
func (p PackInfo) Source() string {
	switch {
	case p.Kind == "git" && p.Commit != "":
		return "git+" + normalizeRemote(p.Remote) + "@" + p.Commit
	case p.Kind == "registry" && p.Version != "":
		return "registry:" + p.ID + "@" + p.Version
	case p.Kind == "local":
		return "local"
	}
	return Unpinned
}

// normalizeRemote makes Manager's and git's spellings of a remote compare
// equal: git@github.com:a/b.git and https://github.com/a/b/ → https://github.com/a/b.
func normalizeRemote(r string) string {
	if rest, ok := strings.CutPrefix(r, "git@"); ok {
		host, path, _ := strings.Cut(rest, ":")
		r = "https://" + host + "/" + path
	}
	r = strings.TrimSuffix(strings.TrimSuffix(r, "/"), ".git")
	return r
}

// ServerPacks is what the server reported, keyed by pack directory.
type ServerPacks struct {
	From  string // "comfyform-helper" or "ComfyUI-Manager"
	Packs map[string]PackInfo
}

var commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Packs asks the comfyform helper for installed packs, falling back to
// ComfyUI-Manager (git packs only). It returns nil when neither answers.
func (c *Client) Packs(ctx context.Context) *ServerPacks {
	if b, err := c.get(ctx, "/comfyform/packs"); err == nil {
		var r struct {
			Packs map[string]PackInfo `json:"packs"`
		}
		if json.Unmarshal(b, &r) == nil && r.Packs != nil {
			return &ServerPacks{From: "comfyform-helper", Packs: r.Packs}
		}
	}
	b, err := c.get(ctx, "/v2/customnode/installed")
	if err != nil {
		return nil
	}
	var m map[string]struct {
		Ver   string `json:"ver"`
		CnrID string `json:"cnr_id"`
		AuxID string `json:"aux_id"`
	}
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	sp := &ServerPacks{From: "ComfyUI-Manager", Packs: map[string]PackInfo{}}
	for dir, e := range m {
		ver := strings.TrimSpace(e.Ver)
		switch {
		case commitRe.MatchString(ver) && e.AuxID != "":
			sp.Packs[dir] = PackInfo{Kind: "git", Remote: "https://github.com/" + e.AuxID, Commit: ver}
		case ver != "" && e.CnrID != "":
			sp.Packs[dir] = PackInfo{Kind: "registry", ID: e.CnrID, Version: ver}
		}
	}
	return sp
}

// PackDir returns the custom_nodes directory a python module comes from:
// "custom_nodes.was-node-suite-comfyui" → "was-node-suite-comfyui". Core
// modules return "".
func PackDir(module string) string {
	rest, ok := strings.CutPrefix(module, "custom_nodes.")
	if !ok {
		return ""
	}
	dir, _, _ := strings.Cut(rest, ".")
	return dir
}
