# comfyform

ComfyUI workflows as Go code, with a Terraform/Pulumi-style workflow: generate typed bindings from your server, write workflows as programs, then `plan` and `apply` them.

```
$ comfy plan -stack preview
Stack preview · 8 nodes · http://127.0.0.1:8188

  ~ base.lora_0                  LoraLoader
      strength_model: 0.8 → 0.65
      strength_clip: 0.8 → 0.65
  ↻ gen.neg                      upstream changed
  ↻ gen.pos                      upstream changed
  ~ gen.sampler                  KSampler
      steps: 12 → 16
  ↻ out.decode                   upstream changed
  ↻ out.save                     upstream changed
  = 2 unchanged

Plan: 0 to add, 2 to change, 4 to rerun, 0 to remove, 2 unchanged.
```

## How it maps

| IaC | comfyform |
|---|---|
| provider | a ComfyUI server; `comfy gen` turns its `/object_info` into typed Go |
| resource | a node, created with a generated constructor |
| component | any Go function taking a `*comfy.Context` (`ctx.Child("hires")` namespaces its nodes) |
| stack | `comfy.<stack>.json`, a config object your program reads |
| plan | compile, diff against the last apply, validate against the live server, check lock drift |
| apply | queue, wait, download outputs to `out/<stack>/<node>/` |
| state | `.comfy/state/<stack>.json`: last applied graph + Merkle hashes per node |
| lockfile | `comfy.lock.json`: interface hash + python module per node class, the custom node packs they come from, and referenced model files |

## Quick start

```sh
go install github.com/erstaples/comfyform/cmd/comfy@latest

mkdir myflow && cd myflow && go mod init myflow
go get github.com/erstaples/comfyform
comfy gen -server http://gpu-box:8188          # writes nodes/nodes.gen.go

# start from an existing workflow: in ComfyUI, Workflow → Export (API)
comfy import -o main.go my_workflow_api.json
echo '{"server": "http://gpu-box:8188"}' > comfy.json

comfy plan
comfy apply
comfy lock
```

No GPU handy? `go run ./cmd/fakecomfy` serves a fake API from `testdata/object_info.json`, and `examples/cabin` runs against it.

## Writing workflows

```go
comfy.Run(func(ctx *comfy.Context) error {
    cfg := ctx.Config()
    base := stages.Checkpoint(ctx.Child("base"), "sdxl.safetensors")
    m := stages.WithLoras(ctx.Child("base"), base, stages.Lora{Name: "ghibli_wc.safetensors", Weight: 0.8})

    lat := stages.Txt2Img(ctx.Child("gen"), m, gen)
    if cfg.Bool("hires") {
        lat = stages.Hires(ctx.Child("hires"), m, lat, gen, 1.5, 0.45)
    }
    stages.Save(ctx.Child("out"), m, lat, "cabin_"+ctx.Stack(), cfg.Bool("grain"))
    return nil
})
```

Node ids are the dotted names (`hires.sampler`), so diffs stay meaningful across edits, and the same ids show up as node titles in ComfyUI's queue and history.

### Binding conventions

- Links are typed: `graph.Ref[nodes.MODEL]`. Wiring a CLIP into a MODEL input is a compile error. Any-typed sockets (`*`) take `graph.AnyRef`.
- A required input left unwired is a build error that names the node.
- **Zero means default.** `Steps: 0` sends the schema default (20). To send a literal zero, use `Raw: graph.Inputs{"strength_clip": 0}`. `comfy import` does this for you.
- `BOOLEAN` fields are `*bool` (`graph.Bool(false)`), since `false` is a common non-default.
- `Raw` also wires a link into a widget input: `Raw: graph.Inputs{"seed": seedNode.Int}`.
- Classes you haven't generated bindings for: `ctx.Node(name, "Class Name", graph.Inputs{...}, nil)`.
- Model file combos (`ckpt_name`, `lora_name`, ...) get no implied default. Silently picking whichever file sorts first is a trap.

## What plan checks

- Every class is installed; required inputs are present.
- Link types are compatible; slot indices exist.
- Combo values are valid. For model files, that doubles as "is this model on the server".
- Numeric bounds.
- Drift: node classes whose interface or python module changed since `comfy lock`, and custom node packs now at a different commit or version, or with edited files.

Apply refuses to queue when there are errors, and skips the server entirely when every output node's Merkle hash matches the last apply and the files are on disk (`-force` overrides).

## Commands

```
comfy gen     [-server URL | -schema object_info.json] [-o nodes]
comfy import  [-nodes ./nodes] [-o main.go] workflow_api.json
comfy build   [-stack s] [-o prompt.json]
comfy plan    [-stack s] [-v]
comfy apply   [-stack s] [-auto-approve] [-force]
comfy lock
comfy graph   [-stack s] [-format mermaid|dot]
comfy stacks
```

Server resolution: `-server` > stack file `"server"` > `comfy.json` > `$COMFY_SERVER` > `http://127.0.0.1:8188`.

## Known gaps / next steps

- **Lock** pins custom node packs (git commit, Registry version, or content hash) when the server runs the [comfyform helper](helper/README.md), and git commits only via ComfyUI-Manager otherwise. Model files are pinned by *name*; hashes could come from the same helper.
- **Progress** is polled from `/history`. Switching to the websocket would give per-node progress and previews.
- **Output cache** is per stack and per apply. A content-addressed store keyed by node hash would let stacks share results.
- **UI export** (UI-format JSON with auto-layout) isn't implemented; `comfy graph` gives a Mermaid/DOT view instead.
- Node ids contain dots. ComfyUI's API treats ids as opaque strings, but if a custom node chokes on that, ids are easy to remap at queue time.
