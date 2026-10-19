// Command comfy is workflow-as-code for ComfyUI: generate typed bindings from
// a server, write workflows in Go, and plan/apply them like infrastructure.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/erstaples/comfyform/codegen"
	"github.com/erstaples/comfyform/engine"
	"github.com/erstaples/comfyform/graph"
	"github.com/erstaples/comfyform/schema"
)

const usage = `comfy - ComfyUI workflows as code

Usage:
  comfy gen     [-server URL | -schema object_info.json] [-o nodes] [-pkg nodes]
  comfy import  [-server URL | -schema file] [-nodes ./nodes] [-o main.go] workflow_api.json
  comfy build   [-stack name] [-o prompt.json]     compile to API JSON
  comfy plan    [-stack name] [-v]                 diff + validate against the server
  comfy apply   [-stack name] [-auto-approve] [-force]
  comfy lock    [-stack name]                      pin node interfaces and model files
  comfy graph   [-stack name] [-format mermaid|dot]
  comfy stacks

Common flags: -dir (project dir, default "."), -server (overrides comfy.json and stack files).
Server defaults to $COMFY_SERVER or http://127.0.0.1:8188.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cmds := map[string]func(context.Context, []string) error{
		"gen": cmdGen, "import": cmdImport, "build": cmdBuild, "plan": cmdPlan,
		"apply": cmdApply, "lock": cmdLock, "graph": cmdGraph, "stacks": cmdStacks,
	}
	name := os.Args[1]
	fn, ok := cmds[name]
	if !ok {
		if name == "-h" || name == "--help" || name == "help" {
			fmt.Print(usage)
			return
		}
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", name, usage)
		os.Exit(2)
	}
	if err := fn(ctx, os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

type common struct {
	dir, stack, server string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.dir, "dir", ".", "project directory")
	fs.StringVar(&c.stack, "stack", "default", "stack name (comfy.<stack>.json)")
	fs.StringVar(&c.server, "server", "", "ComfyUI server URL")
}

// session is a loaded project + stack + compiled prompt.
type session struct {
	proj   *engine.Project
	sf     *engine.StackFile
	stack  string
	server string
	client *engine.Client
}

func open(c common) (*session, error) {
	proj, err := engine.LoadProject(c.dir)
	if err != nil {
		return nil, err
	}
	sf, err := proj.Stack(c.stack)
	if err != nil {
		return nil, err
	}
	server := proj.ServerFor(c.server, sf)
	return &session{proj: proj, sf: sf, stack: c.stack, server: server, client: engine.NewClient(server)}, nil
}

// --- gen ---

func cmdGen(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	server := fs.String("server", "", "ComfyUI server URL")
	schemaFile := fs.String("schema", "", "read /object_info from a file instead of a server")
	out := fs.String("o", "nodes", "output directory for bindings")
	pkg := fs.String("pkg", "", "package name (default: base of -o)")
	dir := fs.String("dir", ".", "project directory (schema cache goes in <dir>/.comfy)")
	_ = fs.Parse(args)

	raw, src, err := readSchema(ctx, *server, *schemaFile, *dir)
	if err != nil {
		return err
	}
	s, err := schema.Parse(raw)
	if err != nil {
		return err
	}
	if *pkg == "" {
		*pkg = filepath.Base(filepath.Clean(*out))
	}
	code, err := codegen.Generate(s, *pkg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	path := filepath.Join(*out, "nodes.gen.go")
	if err := os.WriteFile(path, code, 0o644); err != nil {
		return err
	}
	cache := filepath.Join(*dir, engine.StateDir, "object_info.json")
	_ = os.MkdirAll(filepath.Dir(cache), 0o755)
	_ = os.WriteFile(cache, raw, 0o644)
	fmt.Printf("Generated %d node bindings from %s → %s\n", len(s.Classes), src, path)
	return nil
}

func readSchema(ctx context.Context, server, file, dir string) ([]byte, string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		return b, file, err
	}
	if server == "" {
		if proj, err := engine.LoadProject(dir); err == nil {
			server = proj.ServerFor("", &engine.StackFile{})
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	b, err := engine.NewClient(server).ObjectInfo(cctx)
	if err != nil {
		return nil, "", fmt.Errorf("fetch /object_info from %s: %w", server, err)
	}
	return b, server, nil
}

// --- import ---

func cmdImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	server := fs.String("server", "", "ComfyUI server URL")
	schemaFile := fs.String("schema", "", "object_info.json (default: <dir>/.comfy cache, then server)")
	nodesDir := fs.String("nodes", "./nodes", "directory of generated bindings")
	out := fs.String("o", "", "write Go source here (default stdout)")
	dir := fs.String("dir", ".", "project directory")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: comfy import [flags] workflow_api.json")
	}
	wf, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	p, err := engine.ParseWorkflow(wf)
	if err != nil {
		return err
	}
	if *schemaFile == "" && *server == "" {
		if c := filepath.Join(*dir, engine.StateDir, "object_info.json"); fileExists(c) {
			*schemaFile = c
		}
	}
	raw, _, err := readSchema(ctx, *server, *schemaFile, *dir)
	if err != nil {
		return err
	}
	s, err := schema.Parse(raw)
	if err != nil {
		return err
	}
	imp, err := goImportPath(*nodesDir)
	if err != nil {
		return err
	}
	src, err := engine.Import(p, s, engine.ImportOptions{NodesImport: imp, NodesPkg: filepath.Base(filepath.Clean(*nodesDir))})
	if err != nil {
		return err
	}
	if *out == "" {
		_, err = os.Stdout.Write(src)
		return err
	}
	if err := os.WriteFile(*out, src, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Imported %d nodes → %s\n", len(p), *out)
	return nil
}

// goImportPath derives the import path of dir from the enclosing go.mod.
func goImportPath(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for d := abs; ; d = filepath.Dir(d) {
		b, err := os.ReadFile(filepath.Join(d, "go.mod"))
		if err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if f := strings.Fields(line); len(f) == 2 && f[0] == "module" {
					rel, _ := filepath.Rel(d, abs)
					if rel == "." {
						return f[1], nil
					}
					return f[1] + "/" + filepath.ToSlash(rel), nil
				}
			}
			return "", fmt.Errorf("no module line in %s/go.mod", d)
		}
		if filepath.Dir(d) == d {
			return "", fmt.Errorf("no go.mod above %s; run go mod init first", abs)
		}
	}
}

// --- build / plan / apply ---

func cmdBuild(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	var c common
	c.register(fs)
	out := fs.String("o", "", "write API JSON here (default stdout)")
	_ = fs.Parse(args)
	ss, err := open(c)
	if err != nil {
		return err
	}
	p, err := ss.proj.Compile(ctx, ss.stack, ss.sf)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	b = append(b, '\n')
	if *out == "" {
		_, err = os.Stdout.Write(b)
		return err
	}
	return os.WriteFile(*out, b, 0o644)
}

func cmdPlan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	var c common
	c.register(fs)
	verbose := fs.Bool("v", false, "show unchanged nodes and all inputs of added nodes")
	_ = fs.Parse(args)
	ss, err := open(c)
	if err != nil {
		return err
	}
	pl, _, _, err := ss.plan(ctx, *verbose)
	if err != nil {
		return err
	}
	if pl.HasErrors() {
		return errors.New("plan has errors")
	}
	return nil
}

func (ss *session) plan(ctx context.Context, verbose bool) (*engine.Plan, *engine.State, *schema.Schema, error) {
	p, err := ss.proj.Compile(ctx, ss.stack, ss.sf)
	if err != nil {
		return nil, nil, nil, err
	}
	s, live, err := ss.proj.LoadSchema(ctx, ss.client)
	if err != nil {
		return nil, nil, nil, err
	}
	prev, err := ss.proj.LoadState(ss.stack)
	if err != nil {
		return nil, nil, nil, err
	}
	lock, err := ss.proj.LoadLock()
	if err != nil {
		return nil, nil, nil, err
	}
	var packs *engine.ServerPacks
	if live {
		packs = ss.client.Packs(ctx)
	}
	pl, err := engine.MakePlan(prev, p, s, lock, packs)
	if err != nil {
		return nil, nil, nil, err
	}
	pr := engine.NewPrinter(os.Stdout)
	src := ss.server
	if !live {
		src = "cached schema (server unreachable: " + ss.server + ")"
	}
	fmt.Printf("Stack %s · %d nodes · %s\n\n", ss.stack, len(p), src)
	pr.Plan(pl, verbose)
	if lock == nil {
		fmt.Println("\nNo lockfile yet. Run `comfy lock` to pin node interfaces and model files.")
	}
	return pl, prev, s, nil
}

func cmdApply(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	var c common
	c.register(fs)
	auto := fs.Bool("auto-approve", false, "skip confirmation")
	force := fs.Bool("force", false, "queue even if outputs are current")
	_ = fs.Parse(args)
	ss, err := open(c)
	if err != nil {
		return err
	}
	pl, prev, s, err := ss.plan(ctx, false)
	if err != nil {
		return err
	}
	if pl.HasErrors() {
		return errors.New("plan has errors; not applying")
	}
	p, err := ss.proj.Compile(ctx, ss.stack, ss.sf) // cached by go build, cheap
	if err != nil {
		return err
	}
	outNodes := engine.OutputNodes(p, s)
	if !*force && prev.OutputsCurrent(ss.proj.Dir, pl.Hashes, outNodes) {
		fmt.Println("\nNo changes. Outputs are current (use -force to re-queue).")
		return nil
	}
	if !*auto {
		fmt.Print("\nQueue this on " + ss.server + "? [y/N] ")
		ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			return errors.New("cancelled")
		}
	}

	id, err := ss.client.Queue(ctx, p)
	if err != nil {
		return err
	}
	fmt.Printf("\nQueued %s ", id)
	res, err := ss.client.Wait(ctx, id, func(time.Duration) { fmt.Print(".") })
	fmt.Println()
	if err != nil {
		return err
	}

	st := &engine.State{
		Stack: ss.stack, AppliedAt: time.Now().UTC(), Server: ss.server, PromptID: id,
		Prompt: p, Hashes: pl.Hashes, Outputs: map[string][]engine.OutputFile{},
	}
	n := 0
	for _, node := range outNodes {
		for _, f := range res.Outputs[node] {
			data, err := ss.client.Download(ctx, f)
			if err != nil {
				return fmt.Errorf("download %s: %w", f.Filename, err)
			}
			rel := filepath.Join("out", ss.stack, node, f.Filename)
			if err := os.MkdirAll(filepath.Join(ss.proj.Dir, filepath.Dir(rel)), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(ss.proj.Dir, rel), data, 0o644); err != nil {
				return err
			}
			st.Outputs[node] = append(st.Outputs[node], engine.OutputFile{Path: rel, Hash: pl.Hashes[node]})
			fmt.Printf("  %s\n", rel)
			n++
		}
	}
	if err := ss.proj.SaveState(st); err != nil {
		return err
	}
	fmt.Printf("Apply complete: %d file(s) from %d output node(s).\n", n, len(outNodes))
	return nil
}

func cmdLock(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("lock", flag.ExitOnError)
	var c common
	c.register(fs)
	_ = fs.Parse(args)
	ss, err := open(c)
	if err != nil {
		return err
	}
	s, live, err := ss.proj.LoadSchema(ctx, ss.client)
	if err != nil {
		return err
	}
	if !live {
		return fmt.Errorf("lock needs a live server (%s unreachable)", ss.server)
	}
	stacks := ss.proj.Stacks()
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "stack" })
	if set {
		stacks = []string{c.stack}
	}
	ps := make([]graph.Prompt, 0, len(stacks))
	for _, st := range stacks {
		sf, err := ss.proj.Stack(st)
		if err != nil {
			return err
		}
		p, err := ss.proj.Compile(ctx, st, sf)
		if err != nil {
			return err
		}
		ps = append(ps, p)
	}
	packs := ss.client.Packs(ctx)
	l := engine.BuildLock(ps, s, ss.client.Version(ctx), packs)
	if err := ss.proj.SaveLock(l); err != nil {
		return err
	}
	files := 0
	for _, v := range l.Files {
		files += len(v)
	}
	var unpinned []string
	for dir, lp := range l.Packs {
		if lp.Source == engine.Unpinned {
			unpinned = append(unpinned, dir)
		}
	}
	sort.Strings(unpinned)
	fmt.Printf("Locked %d node classes, %d model/input files and %d custom node packs across stacks: %s\n",
		len(l.Classes), files, len(l.Packs), strings.Join(stacks, ", "))
	switch {
	case len(l.Packs) == 0:
	case packs == nil:
		fmt.Println("Packs are unpinned: the server has neither the comfyform helper nor ComfyUI-Manager (https://github.com/erstaples/comfyform-helper).")
	case len(unpinned) > 0:
		fmt.Printf("Unpinned (%s can't identify them): %s\n", packs.From, strings.Join(unpinned, ", "))
		if packs.From != "comfyform-helper" {
			fmt.Println("Install the comfyform helper to pin these (https://github.com/erstaples/comfyform-helper).")
		}
	}
	return nil
}

func cmdGraph(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph", flag.ExitOnError)
	var c common
	c.register(fs)
	format := fs.String("format", "mermaid", "mermaid or dot")
	_ = fs.Parse(args)
	ss, err := open(c)
	if err != nil {
		return err
	}
	p, err := ss.proj.Compile(ctx, ss.stack, ss.sf)
	if err != nil {
		return err
	}
	s, _, _ := ss.proj.LoadSchema(ctx, ss.client)
	switch *format {
	case "dot":
		fmt.Print(engine.DOT(p, s))
	case "mermaid":
		fmt.Print(engine.Mermaid(p, s))
	default:
		return fmt.Errorf("unknown format %q", *format)
	}
	return nil
}

func cmdStacks(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stacks", flag.ExitOnError)
	dir := fs.String("dir", ".", "project directory")
	_ = fs.Parse(args)
	proj, err := engine.LoadProject(*dir)
	if err != nil {
		return err
	}
	for _, st := range proj.Stacks() {
		state, _ := proj.LoadState(st)
		last := "never applied"
		if state != nil {
			last = "applied " + state.AppliedAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Printf("%-12s %s\n", st, last)
	}
	return nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
