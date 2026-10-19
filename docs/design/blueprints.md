# Blueprints

A blueprint is a named, versioned piece of ComfyUI graph with a declared
interface: input and output sockets (ports) and parameters (settings). It is
written either as a saved group of nodes or as a Starlark program. A graph uses
a blueprint by holding an instance of it, a node whose class is
`bp:<namespace>/<name>@<version>`. `blueprint.Compile` expands every instance
into ordinary nodes, so ComfyUI receives a plain API-format graph and never sees
a blueprint.

The `blueprint` package (`github.com/erstaples/comfyform/blueprint`) reads and
checks packages, computes digests and interface differences, and compiles. It
builds on `graph` (the API-format graph) and `schema` (object_info and
`Class.Hash`).

This page is the format reference. Couchy, the phone app that stores, shares and
runs blueprints, is described in
`docs/superpowers/specs/2026-10-19-blueprints-design.md` in the comfy-mobile
repository; its storage, endpoints and screens are not covered here.

## The package API

| Name | Does |
|---|---|
| `Load(fs.FS) (*Package, error)` | reads a package, computes its digest, runs `Check(nil)` |
| `(*Package).Check(Classes) Problems` | every problem the package has; given a server's classes it also compares the types a repeat's carries join |
| `ParseManifest([]byte) (*Manifest, error)` | decodes a `blueprint.json` and runs the checks that need no other file |
| `Digest(map[string][]byte) string` | the digest of a set of files |
| `ParseClass(string)`, `(*Manifest).Class()` | convert between an id and version and a `bp:` class name |
| `Compile(graph.Prompt, Options) (*Result, error)` | expands the instances of a graph |
| `Diff`, `LevelOf`, `CheckVersion` | interface differences and version bumps |
| `Classes`, `SchemaClasses(*schema.Schema)` | what compile and the checks need to know of a server's node classes |
| `Resolver` | how compile finds a package by digest |

A host supplies a `Resolver` (`Package(digest string) (*Package, error)`) and,
for programs and type checks, `Classes`. `SchemaClasses` adapts a parsed
`/object_info`; another tool adapts its own node library to the interface.

```go
pkg, err := blueprint.Load(os.DirFS("reference-images"))

res, err := blueprint.Compile(prompt, blueprint.Options{
    Classes:  blueprint.SchemaClasses(s),
    Packages: store, // implements Package(digest)
})
```

## Package

A package is a directory:

```
reference-images/
  blueprint.json     manifest (required)
  graph.json         node group, or
  main.star          program
  README.md          optional
  cover.png          optional
```

Exactly one of `graph.json` and `main.star`.

`Load` takes any `fs.FS` whose root is the package: `os.DirFS(dir)` and a
`*zip.Reader` both work, and `fs.Sub` selects a package inside a larger tree.

- Names that start with `.`, and everything under a directory that does, are
  ignored: they do not count toward the digest or the size cap.
- Symlinks and other non-regular files are refused.
- The files add up to at most `MaxPackageBytes` (1 MiB).

`Load` returns `(nil, Problems)` when a check fails. `Problems` is an error that
holds a `Problem{Where, Msg}` per finding, one per line when printed; get the
list with `errors.As`. A package that cannot be read at all (a missing
`blueprint.json`, JSON that does not parse, an unknown manifest field, a
symlink, a package over the cap) is a plain error.

**Digest.** `"sha256:"` and the hex of a sha256 over, for each counted file in
order of slash path, `path`, a NUL byte, the hex sha256 of the file's bytes and a
newline. It is the construction comfyform-helper uses for a node pack's content.
Every counted file takes part, `README.md` and `cover.png` included, so editing
the description changes the digest.

## Manifest

`blueprint.json` is decoded with unknown fields refused. This is the manifest of
a node group with one repeat:

```json
{
  "format": 1,
  "id": "erstaples/reference-images",
  "version": "1.0.0",
  "title": "Reference images",
  "description": "Adds each picked image to the conditioning through Reference Latent.",
  "inputs": [
    { "name": "conditioning", "type": "CONDITIONING", "to": [{ "nodeId": "4", "input": "conditioning" }] },
    { "name": "vae", "type": "VAE", "to": [{ "nodeId": "3", "input": "vae" }] }
  ],
  "outputs": [
    { "name": "conditioning", "type": "CONDITIONING", "from": { "nodeId": "4", "output": 0 } }
  ],
  "settings": [
    { "name": "images", "kind": "image", "list": { "min": 0, "max": 8 },
      "to": [{ "nodeId": "1", "input": "image" }] },
    { "name": "megapixels", "kind": "float", "default": 1.0, "min": 0.25, "max": 4, "step": 0.05,
      "to": [{ "nodeId": "2", "input": "megapixels" }] },
    { "name": "upscale_method", "kind": "combo", "options": ["lanczos", "bicubic", "area"], "default": "lanczos",
      "to": [{ "nodeId": "2", "input": "upscale_method" }] }
  ],
  "repeats": [{
    "nodes": ["1", "2", "3", "4"],
    "over": ["images"],
    "carry": [{ "to":   { "nodeId": "4", "input": "conditioning" },
                "from": { "nodeId": "4", "output": 0 } }]
  }],
  "requires": {
    "classes": {
      "LoadImage": "sha256:24173251fa3ff4dd406e6e09",
      "ImageScaleToTotalPixels": "sha256:db95bb40ca557aa4fb76a317",
      "VAEEncode": "sha256:30d9a0d5a96b9750e7398b9a",
      "ReferenceLatent": "sha256:e429f796976a6c46e49076be"
    }
  },
  "uses": {}
}
```

| Field | Meaning |
|---|---|
| `format` | `1`, the only format this build reads |
| `id` | `<namespace>/<name>`, both `[a-z0-9-]+` |
| `version` | `MAJOR.MINOR.PATCH`, digits only |
| `title` | required, not blank |
| `description` | optional |
| `inputs`, `outputs` | ports |
| `settings` | parameters |
| `repeats` | node group only; see Repeats |
| `result` | optional; the inner output node (a Save Image, say) whose images a host collects when it runs an instance of this blueprint |
| `requires` | what a server needs |
| `uses` | blueprints this one instantiates, pinned |

### Ports

A port is `{name, type}` plus `optional` for an input. `type` is a ComfyUI
socket type; `*` matches any type, and two types match when they are equal or
either is `*`. An optional input may have nothing behind it: a program sees
`None` for it, and a node group drops the inputs it feeds.

Port and setting names match `[a-z][a-z0-9_]*`. Input ports and settings become the inputs of an
instance, so they share one set of names; output ports have a set of their own.
An instance links output *i* of its node, *i* being the port's position in
`outputs`.

### Settings

A setting is a value a person gives an instance, as opposed to a link.

| Field | Meaning |
|---|---|
| `kind` | `int`, `float`, `string`, `bool`, `combo`, `image` (an uploaded file's name) or `seed` |
| `default` | required, except for an `image` and for a list, which start empty (`""` and `[]`) |
| `min`, `max`, `step` | bounds for `int`, `float` and `seed`; `min` must not exceed `max`, `step` must be above 0 |
| `options` | the values of a `combo` |
| `source` | `{"kind": "models", "folder": "..."}`, for a `combo` whose options are a server's model files; compile does not check the value |
| `list` | `{"min": n, "max": n}`: the setting is a list of `kind` with that many items. Not for `seed` |
| `linkable` | whether a link in the graph may feed the setting |
| `to` | node group only: the node inputs the setting fills |

A `combo` needs `options` or a `source`. An `int` holds a whole number and a
`seed` a whole number of 0 or more. A `default` is checked against the kind and
bounds; a list's default must be a list within `list` bounds.

A setting is a value unless it is `linkable`. `seed` is linkable by default;
set `"linkable": false` to turn that off. A list cannot be linkable. A linked
setting arrives in a program as a socket reference, which it may pass to
`ctx.node` or to a linkable setting of a nested blueprint, and nothing else.
In a node group a linked setting feeds the inputs its `to` names, and a nested
instance's linkable settings.

### Requires

| Field | Meaning |
|---|---|
| `classes` | each ComfyUI class the blueprint may emit, mapped to its interface hash: `"sha256:"` and 24 hex digits, the value of `schema.Class.Hash` on the server the blueprint was made against |
| `packs` | `dir` to `{source, content}`, the form of `comfy.lock.json`'s `packs` |
| `models` | `"Class.input"` to the model file names it needs |

`Compile` refuses to emit a class that is not in `requires.classes`, whatever
settings a program receives, so a host can check a server against the manifest
without running the program. Comparing `classes`, `packs` and `models` with a
server is the host's job; this package checks their shape and carries them.

### Uses

`uses` maps the id of each blueprint this one instantiates to
`{"version": "1.0.0", "digest": "sha256:..."}`, the digest being 64 hex digits.
A package is complete only with the packages its `uses` name; `Load` does not
resolve them, `Compile` does through the `Resolver`.

### Result

`result` names a node of the instance. In a node group it is a node id of
`graph.json` outside any repeat; in a program it is the dot-separated path of the
child names and then the node name, such as `ref0.encode`. `Result.Results` maps
each instance whose blueprint has a `result` to the compiled id of that node,
which is `<instance id>.<result>`.

## Node group form

`graph.json` holds API-format nodes: `class_type`, `inputs`, and `_meta`. The
manifest says where ports and settings go, with `to` and `from`:

- An input port's `to` lists `{nodeId, input}` targets; one port may feed
  several.
- A setting's `to` lists the same. The input must exist in the node, so
  `graph.json` holds a placeholder value there, usually the default.
- An output port's `from` is `{nodeId, output}`.

A link to the id `in` (`["in", 0]`) marks an input that a port fills; compile
replaces it with the port's socket. `in` is reserved and cannot be a node id. An
`["in", n]` link that no port fills is a compile error.

```json
{
  "1": { "class_type": "LoadImage", "inputs": { "image": "" } },
  "2": { "class_type": "ImageScaleToTotalPixels",
         "inputs": { "image": ["1", 0], "upscale_method": "lanczos", "megapixels": 1.0, "resolution_steps": 1 } },
  "3": { "class_type": "VAEEncode", "inputs": { "pixels": ["2", 0], "vae": ["in", 0] } },
  "4": { "class_type": "ReferenceLatent", "inputs": { "conditioning": ["in", 0], "latent": ["3", 0] } }
}
```

Every node's class must be in `requires.classes`. A node whose class is a `bp:`
class is an instance of another blueprint: the manifest's `uses` pins that id at
that version, and the node's `_meta.blueprint` is the pinned digest. The nested
instance gets its ports and settings from the node's inputs and from the
bindings that point at it; it may sit inside a repeat, which is how repeats
nest.

A list setting must drive a repeat, so `Check` reports one in a node group that
drives none.

## Repeats

A node group repeats part of itself with `repeats`. Each entry names some of the
group's nodes, what makes the copies, and which sockets pass from one copy to
the next.

| Field | Meaning |
|---|---|
| `nodes` | the repeated node ids |
| `over` | list settings; copy *i* gets element *i* of each. Several must have the same length |
| `count` | instead of `over`: an int setting, and that many copies |
| `carry` | `{to: {nodeId, input}, from: {nodeId, output}}`: output of copy *i* into input of copy *i + 1* |

An entry has `over` or `count`, not both. A carry may carry any socket type, and
a repeat may carry several.

How the copies fit together:

- Copy *k* gets ids `<prefix>.r<k>.<node id>`. The first copy's carried input
  takes whatever that input links to in the group (a node outside the repeat or
  an input port); each later copy takes the carried output of the copy before.
- The last copy's carried output is what everything after the repeat sees.
- Links between repeated nodes stay inside a copy. Links from nodes outside the
  repeat, and from input ports, reach every copy. Settings not in `over` give
  every copy the same value.
- With no copies (an empty list, or a count of 0) the repeated nodes are left
  out, and what took a carried output takes what the carried input linked to, so
  the chain passes straight through.
- `over` lists of different lengths fail the compile. A `count` must be a whole
  number from 0 to `MaxNodes`, and cannot be linked.
- Copy *k* has the source map item `{setting: <first over setting or the count
  setting>, index: k}`.

Rules `Check` enforces:

- Only carried outputs leave a repeat. A repeated node's output that feeds a node
  outside the repeat, or an output port, must be a carry's `from`. Output nodes
  inside a repeat (a Save Image per copy) are fine. `result` cannot be inside a
  repeat.
- A carry's ends are in the repeat. Its `to` input is linked from outside the
  repeat, by a port or by a link to a node outside it, and is carried once.
  Given `Classes`, the `from` output type and the `to` input type match.
- No path leaves a repeat and comes back into it.
- Two repeats share no node. An `over` setting drives one repeat, and all its
  `to` targets are in that repeat.
- `count` names an int setting that is not a list.

Two shapes this covers:

| | nodes | copies | carry |
|---|---|---|---|
| Reference images | Load Image, Scale Image to Total Pixels, VAE Encode, Reference Latent | `over: ["images"]` | Reference Latent's `CONDITIONING` into its `conditioning` |
| Refine passes | Upscale Latent By, KSampler | `count: "passes"` | KSampler's `LATENT` into Upscale Latent By's `samples` |

A program is for what a repeat cannot say: branching on a setting, a copy that
differs by more than its list element, or lists of different lengths.

## Starlark programs

`main.star` defines `build(ctx, inputs, settings)` and returns a dict from each
output port name to a socket reference, or to `None` for nothing behind the
port. Every output port must be present, and a reference's type must match the
port's.

```python
def build(ctx, inputs, settings):
    cond = inputs.conditioning
    for i, name in enumerate(settings.images):
        c = ctx.child("ref%d" % i, item = ("images", i))
        img = c.node("load", "LoadImage", image = name).IMAGE
        img = c.node("scale", "ImageScaleToTotalPixels", image = img,
                     upscale_method = settings.upscale_method,
                     megapixels = settings.megapixels).IMAGE
        lat = c.node("encode", "VAEEncode", pixels = img, vae = inputs.vae).LATENT
        cond = c.node("ref", "ReferenceLatent", conditioning = cond, latent = lat).CONDITIONING
    return {"conditioning": cond}
```

With no images the loop does not run and the output is the input port's socket,
so the conditioning passes straight through. Compile points the consumers of an
output that is an input at that input's producer.

| Host call | Does |
|---|---|
| `ctx.node(name, class, **inputs)` | emits a node with id `<prefix>.<name>` and returns a node value |
| `node.<OUTPUT>`, `node.out(i)` | a socket reference to an output, by the output's name from object_info or by index. Two outputs with one name need `out(i)` |
| `ctx.child(name, item = (setting, index))` | a context whose nodes are prefixed with `name`; `item` records which element of a list setting they were built for |
| `ctx.use(id, name, inputs = {...}, settings = {...})` | instantiates a blueprint from `uses` under `name` and returns a struct of its outputs |
| `inputs.<port>` | the socket reference behind that input port, or `None` |
| `settings.<name>` | the setting's value |
| `fail(msg)` | stops the compile with an error on this instance |

**Names.** A node, child or use name matches `[A-Za-z0-9_-]+`: it is one part of
a compiled id, so it has no dot. A duplicate id is an error, not an overwrite.

**Node inputs.** A keyword value is a literal (a string, int, float, bool, or a
list or tuple of them) or a socket reference. `None`, or leaving a widget input
out, takes the class's default from the server's object_info. A literal list of
a string and a number reads as a link and is refused; pass a socket reference.
The class must be on the server (`Options.Classes`) and in `requires.classes`.

**Item.** `item` names a list setting of this blueprint and an index within the
list the instance holds; anything else fails. Without `item` a child is made for
what its parent is made for.

**Use.** `ctx.use` takes `inputs` that map input port names to socket references
or `None`, and `settings` that map setting names to literals, socket references
(for a linkable setting) or `None` for the default. The id must be in `uses`.

**Setting values.** An `int` or `seed` is an int; a `float` a float; `string`,
`combo` and `image` strings; `bool` a bool. A list setting is a frozen list. A
list longer than its `list.max` is rejected before the program runs, as is any
value outside the manifest's kind, bounds or options. A linked setting is a
socket reference; using it for anything but `ctx.node` or a linkable setting of
a `ctx.use` fails with `setting seed is linked; this blueprint needs a value`.
A socket reference supports no arithmetic, comparison or hashing.

## Sandbox

The host predeclares the calls above plus `struct` and `json` (starlark-go's
`json` module, with a `decode` that refuses input nested deeper than 1,000
levels). Starlark's own builtins (`len`, `range`, `enumerate`, `fail` and the
rest) are available. `print` is discarded.

- `load` fails: blueprints compose through `uses`, not files.
- There is no clock, randomness, file access or network.
- The dialect options are all off: no `while`, no recursion, no `set`, no `if`
  or `for` at the top level, no reassigning a global.
- Each program runs on its own thread, with a step limit and a cancel timer
  (Limits).

The same graph, settings and package digests always compile to the same graph.

**What the sandbox does not bound.** It limits Starlark steps and wall-clock
time. It does not limit memory, and it does not limit the work inside a single
builtin call. The step counter and the timer act between steps, so one call such
as `"x" * 1000000000`, a large list repetition, or `json.encode` or `str` on
data the program built nested deeply (both recurse once per level) runs to
completion or to a crash before either can stop it. Exhausting memory and
overflowing the Go stack are fatal to the whole process, and this package cannot
recover from either.

A host that compiles untrusted blueprints should run `Compile` in a separate
process with memory and time limits.

## Instances in a graph

An instance is an entry of the API-format graph:

```json
"14": {
  "class_type": "bp:erstaples/reference-images@1.0.0",
  "inputs": {
    "conditioning": ["7", 0],
    "vae": ["3", 0],
    "images": ["a.png", "b.png"],
    "megapixels": 1.0,
    "upscale_method": "lanczos"
  },
  "_meta": { "title": "Reference images", "blueprint": "sha256:504e..." }
}
```

- `_meta.blueprint` is the package digest; the class names the id and version.
  Compile asks the `Resolver` for the digest and refuses a package whose
  manifest disagrees with the class.
- A port is a link. A setting is a literal, or a link when it is linkable. A
  setting left out takes its default.
- A list setting is a JSON array. Compile reads a list setting as a list and
  never as a link.
- Anything else in `inputs` is an error: an input must be a port or a setting.
- Settings are checked against the manifest: kind, bounds, options (a `models`
  source excepted) and list length. A required port with no link is an error.
- A link to an instance's output, `["14", 0]`, names the port at that position
  in `outputs`.

## Compile

```go
func Compile(p graph.Prompt, opt Options) (*Result, error)
```

| `Options` field | Meaning |
|---|---|
| `Classes` | the server's node classes; programs need it |
| `Packages` | the `Resolver`; a graph with instances needs it |
| `MaxNodes`, `MaxDepth`, `Steps`, `Timeout` | limits; zero takes the default |

`Compile` only reads `Options` and the packages, so one `Options` value can
serve compiles that run at once. It does not change `p`.

A graph with no instances comes back as `p` itself, with no sources. Otherwise:

1. Each instance's package comes from the `Resolver`, which compile asks once per
   instance of the graph and once per compile for each digest a nested instance
   pins. The package it returns must have the digest asked for. Instances
   compile in dependency order (an instance linked to another compiles after
   it); a cycle among them is an error.
2. An instance's ports and settings are read and checked against its manifest,
   then its package expands under the instance's id: a node group's nodes are
   copied with ports and settings substituted and each repeat is unrolled, and
   a program's `build` runs in the sandbox.
3. Every link to an instance's output is replaced by the socket that output
   resolved to, following a port that passes an input straight through.
4. Nodes of the input graph that are not instances are copied across. A compiled
   id equal to an id the input graph already has is an error, so a graph with
   dotted ids can collide.

`Result.Prompt` is normalized: links are `[]any{id, float64}`, the form JSON
decodes them in. Compiling the reference-images instance above with two images
gives, with `20` a KSampler that took `["14", 0]` as its positive conditioning:

```
14.r0.1  LoadImage                 image "a.png"
14.r0.2  ImageScaleToTotalPixels   image ["14.r0.1", 0]
14.r0.3  VAEEncode                 pixels ["14.r0.2", 0], vae ["3", 0]
14.r0.4  ReferenceLatent           conditioning ["7", 0], latent ["14.r0.3", 0]
14.r1.1  LoadImage                 image "b.png"
14.r1.2  ImageScaleToTotalPixels   image ["14.r1.1", 0]
14.r1.3  VAEEncode                 pixels ["14.r1.2", 0], vae ["3", 0]
14.r1.4  ReferenceLatent           conditioning ["14.r0.4", 0], latent ["14.r1.3", 0]
20       KSampler                  positive ["14.r1.4", 0]
```

### Compiled ids

The outer prefix is the instance's own id in the graph.

| Node | Compiled id | Example |
|---|---|---|
| node group node | `<prefix>.<node id>` | `14.3` |
| copy *k* of a repeated node | `<prefix>.r<k>.<node id>` | `14.r1.3` |
| program node | `<prefix>.<child path>.<name>` | `14.ref1.encode` |
| node of a nested instance | the nested prefix, then the above | `14.base.1`, `14.r0.5.2` |

A nested instance's prefix is `<prefix>.<use name or inner node id>`: the name
given to `ctx.use`, or the id of the node that holds the instance in a node
group (inside a repeat's copy, after `r<k>`).

### Source map

`Result.Sources` maps each compiled id to a `Source`. Nodes copied from the input
graph have no entry.

| Field | Meaning |
|---|---|
| `Instances` | the chain of instances from the input graph inward: `["14"]`, or `["14", "base"]` for a node of an instance nested in 14 |
| `Blueprint` | the innermost instance's `id@version` |
| `Item` | `{Setting, Index}`: the list element or repeat copy the node was made for. Absent when there is none |

`Source.Item` names a setting of the outermost instance, `Instances[0]`, the one
the input graph holds: its repeat copy, or the item its program's `ctx.child`
gave. A nested blueprint's own copies and tags do not show in `Source.Item`; its
nodes carry the item of the outer instance they were made in, or none.

```json
"14.r1.2": {
  "instances": ["14"],
  "blueprint": "erstaples/reference-images@1.0.0",
  "item": { "setting": "images", "index": 1 }
}
```

### Errors

A failure about an instance is an `*Error`, placed as precisely as compile
knows:

| Field | Meaning |
|---|---|
| `Instances` | the chain of instances, as in `Source` |
| `Blueprint` | the blueprint that raised it, `id@version` |
| `Setting` | the setting it is about, a setting of `Blueprint` |
| `Item` | the repeat copy or `ctx.child` item in effect where it was raised: one the raising blueprint made itself, else the one inherited from the instance it is nested in |
| `Msg` | the message |

`Error()` writes the instance chain joined with dots, the blueprint in
parentheses, the item as `setting[index]`, and then the message, prefixed with
`setting <name>: ` unless it names the setting already:

```
14 (erstaples/reference-images@1.0.0): setting images: item 1 must be a string, got 3
14 (erstaples/hires@1.0.0): main.star:2: fail: no good
14 (erstaples/hires@1.0.0): main.star:2: setting seed is linked; this blueprint needs a value
```

An error from a program starts its message with the line of `main.star`. A
mistake in `Options` (no `Packages`) is a plain error.

## Limits

| Limit | Default | Set by |
|---|---|---|
| Starlark steps | 10,000,000 per instance | `Options.Steps` |
| Starlark time | 2 s per instance, from a cancel timer | `Options.Timeout` |
| Compiled nodes | 2,000 per compile | `Options.MaxNodes` |
| Nesting | 8 levels of `uses` below the instance in the graph | `Options.MaxDepth` |
| Package size | 1 MiB | `MaxPackageBytes` |
| `json.decode` nesting | 1,000 levels | fixed |

`MaxNodes` counts nodes and nested instance expansions, so repeats and instances
that fan out to no nodes are bounded too. A program that runs out of steps fails
with `Starlark computation cancelled: too many steps`; one that runs out of time
fails with `Starlark computation cancelled: timeout`. The timer cancels the
thread between steps (see Sandbox).

## Versions

`Diff(old, next *Manifest) []Change` lists the differences that matter to a graph
using the blueprint. Ports and settings match by name, so a rename is a removal
and an addition. What a blueprint does inside, and a setting's default, step,
title and description, are not part of the interface.

| Level | Change |
|---|---|
| Major | an input or setting removed or retyped; an input that became required; a required input added; an output removed, retyped or reordered; a setting that became or stopped being a list; a combo's `source` changed; a range narrowed (a raised `min`, a lowered `max`, a shorter list, a combo option removed); a setting no longer linkable; a setting added without a default; `result` changed from one node to another or to none |
| Minor | an optional input added; an input that became optional; an output added after every existing one; a setting added with a default (an image or a list counts); a range widened; a setting now linkable; `result` set where there was none |
| Patch | nothing else |

`LevelOf` is the highest level among changes, `Patch` for none.

`CheckVersion(old, next)` accepts `next` as the successor of `old` when both have
the same id, `next`'s version is above `old`'s, and the step up is at least as
big as the difference needs: a new major number for a break, a new major or
minor number for an addition. Otherwise it returns an error such as
`1.0.1 understates the change: setting "denoise" removed needs a major version`.
