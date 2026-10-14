package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erstaples/comfyform/graph"
	"github.com/erstaples/comfyform/schema"
)

func TestPackSource(t *testing.T) {
	for _, tc := range []struct {
		p    PackInfo
		want string
	}{
		{PackInfo{Kind: "git", Remote: "https://github.com/a/b.git", Commit: "abc"}, "git+https://github.com/a/b@abc"},
		{PackInfo{Kind: "git", Remote: "git@github.com:a/b.git", Commit: "abc"}, "git+https://github.com/a/b@abc"},
		{PackInfo{Kind: "git", Remote: "https://github.com/a/b/", Commit: "abc"}, "git+https://github.com/a/b@abc"},
		{PackInfo{Kind: "registry", ID: "comfyui-impact-pack", Version: "8.28.3"}, "registry:comfyui-impact-pack@8.28.3"},
		{PackInfo{Kind: "local", Content: "sha256:1"}, "local"},
		{PackInfo{Kind: "git"}, Unpinned},
	} {
		if got := tc.p.Source(); got != tc.want {
			t.Errorf("%+v: got %q, want %q", tc.p, got, tc.want)
		}
	}
}

func TestPackDir(t *testing.T) {
	for in, want := range map[string]string{
		"custom_nodes.was-node-suite-comfyui":    "was-node-suite-comfyui",
		"custom_nodes.ComfyUI-KJNodes.nodes.foo": "ComfyUI-KJNodes",
		"nodes":                                  "",
		"comfy_extras.nodes_flux":                "",
	} {
		if got := PackDir(in); got != want {
			t.Errorf("PackDir(%q) = %q, want %q", in, got, want)
		}
	}
}

const commit = "44de705c7b3c2f27d1d1b0d5cfa4e4f4f5c9e001"

func TestClientPacksPrefersHelperThenManager(t *testing.T) {
	helper := `{"packs": {"was": {"kind": "git", "remote": "https://github.com/x/was.git", "commit": "` + commit + `", "content": "sha256:1"}}}`
	manager := `{"was": {"ver": "` + commit + `\n", "cnr_id": "", "aux_id": "x/was", "enabled": true},
	             "reg": {"ver": "1.2.0", "cnr_id": "reg-pack", "aux_id": null, "enabled": true}}`
	serve := func(routes map[string]string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if b, ok := routes[r.URL.Path]; ok {
				w.Write([]byte(b))
				return
			}
			http.NotFound(w, r)
		}))
	}
	ctx := context.Background()

	both := serve(map[string]string{"/comfyform/packs": helper, "/v2/customnode/installed": manager})
	defer both.Close()
	sp := NewClient(both.URL).Packs(ctx)
	if sp == nil || sp.From != "comfyform-helper" || sp.Packs["was"].Content != "sha256:1" {
		t.Fatalf("helper: %+v", sp)
	}

	mgr := serve(map[string]string{"/v2/customnode/installed": manager})
	defer mgr.Close()
	sp = NewClient(mgr.URL).Packs(ctx)
	if sp == nil || sp.From != "ComfyUI-Manager" {
		t.Fatalf("manager: %+v", sp)
	}
	if got := sp.Packs["was"].Source(); got != "git+https://github.com/x/was@"+commit {
		t.Errorf("manager git source %q", got)
	}
	if got := sp.Packs["reg"].Source(); got != "registry:reg-pack@1.2.0" {
		t.Errorf("manager registry source %q", got)
	}

	none := serve(nil)
	defer none.Close()
	if sp := NewClient(none.URL).Packs(ctx); sp != nil {
		t.Errorf("no helper or manager: got %+v, want nil", sp)
	}
}

func packSchema(t *testing.T) *schema.Schema {
	t.Helper()
	cls := func(name, module string) string {
		return `"` + name + `": {"input": {"required": {}}, "output": [], "output_name": [],
			"name": "` + name + `", "display_name": "` + name + `", "category": "test",
			"output_node": true, "python_module": "` + module + `"}`
	}
	s, err := schema.Parse([]byte(`{` + cls("Grain", "custom_nodes.was") + `,` +
		cls("Seed", "custom_nodes.local_seed") + `,` + cls("SaveImage", "nodes") + `}`))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPackLockAndDrift(t *testing.T) {
	s := packSchema(t)
	p := graph.Prompt{
		"a": {Class: "Grain", Inputs: map[string]any{}},
		"b": {Class: "Seed", Inputs: map[string]any{}},
		"c": {Class: "SaveImage", Inputs: map[string]any{}},
	}
	locked := &ServerPacks{Packs: map[string]PackInfo{
		"was": {Kind: "git", Remote: "https://github.com/x/was", Commit: commit, Content: "sha256:1"},
		// local_seed missing: the server couldn't identify it
	}}
	l := BuildLock([]graph.Prompt{p}, s, "", locked)
	if got := l.Packs["was"]; got.Source != "git+https://github.com/x/was@"+commit || got.Content != "sha256:1" {
		t.Errorf("was pin: %+v", got)
	}
	if got := l.Packs["local_seed"].Source; got != Unpinned {
		t.Errorf("local_seed pin: %q, want unpinned", got)
	}
	if len(l.Packs) != 2 {
		t.Errorf("core modules must not be pinned as packs: %+v", l.Packs)
	}

	drift := func(sp *ServerPacks) string {
		var msgs []string
		for _, pr := range Drift(p, s, l, sp) {
			msgs = append(msgs, pr.Msg)
		}
		return strings.Join(msgs, "\n")
	}
	if got := drift(locked); got != "" {
		t.Errorf("same packs: unexpected drift %q", got)
	}
	if got := drift(nil); got != "" {
		t.Errorf("server reported no packs: unexpected drift %q", got)
	}
	moved := &ServerPacks{Packs: map[string]PackInfo{"was": {Kind: "git", Remote: "https://github.com/x/was.git", Commit: "b" + commit[1:], Content: "sha256:2"}}}
	if got := drift(moved); !strings.Contains(got, "pack was is now git+https://github.com/x/was@b") {
		t.Errorf("new commit: got %q", got)
	}
	edited := &ServerPacks{Packs: map[string]PackInfo{"was": {Kind: "git", Remote: "https://github.com/x/was.git", Commit: commit, Content: "sha256:2"}}}
	if got := drift(edited); !strings.Contains(got, "pack was has local changes") {
		t.Errorf("edited files: got %q", got)
	}
	delete(l.Packs, "was")
	if got := drift(locked); !strings.Contains(got, "pack was is not in comfy.lock.json") {
		t.Errorf("old lockfile: got %q", got)
	}
}
