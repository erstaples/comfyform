// Package stages holds reusable workflow components. Each one takes a child
// context, so its nodes get stable ids like "hires.sampler".
package stages

import (
	"fmt"

	"github.com/erstaples/comfyform/comfy"
	"github.com/erstaples/comfyform/examples/cabin/nodes"
	"github.com/erstaples/comfyform/graph"
)

// Model is the MODEL/CLIP/VAE triple most stages pass around.
type Model struct {
	Model graph.Ref[nodes.MODEL]
	Clip  graph.Ref[nodes.CLIP]
	VAE   graph.Ref[nodes.VAE]
}

func Checkpoint(ctx *comfy.Context, ckpt string) Model {
	c := nodes.CheckpointLoaderSimple(ctx, "ckpt", nodes.CheckpointLoaderSimpleArgs{CkptName: ckpt})
	return Model{Model: c.Model, Clip: c.Clip, VAE: c.VAE}
}

type Lora struct {
	Name   string
	Weight float64
}

// WithLoras chains LoRA loaders onto a model; ids are "<ctx>.lora_0", ...
func WithLoras(ctx *comfy.Context, m Model, loras ...Lora) Model {
	for i, l := range loras {
		out := nodes.LoraLoader(ctx, fmt.Sprintf("lora_%d", i), nodes.LoraLoaderArgs{
			Model: m.Model, Clip: m.Clip, LoraName: l.Name,
			StrengthModel: l.Weight, StrengthClip: l.Weight,
		})
		m.Model, m.Clip = out.Model, out.Clip
	}
	return m
}

type Gen struct {
	Prompt, Negative   string
	Seed, Steps        int64
	CFG                float64
	Sampler, Scheduler string
	Width, Height      int64
}

func (g Gen) encode(ctx *comfy.Context, m Model) (pos, neg graph.Ref[nodes.CONDITIONING]) {
	pos = nodes.CLIPTextEncode(ctx, "pos", nodes.CLIPTextEncodeArgs{Text: g.Prompt, Clip: m.Clip}).Conditioning
	neg = nodes.CLIPTextEncode(ctx, "neg", nodes.CLIPTextEncodeArgs{Text: g.Negative, Clip: m.Clip}).Conditioning
	return
}

// Txt2Img samples a fresh latent.
func Txt2Img(ctx *comfy.Context, m Model, g Gen) graph.Ref[nodes.LATENT] {
	pos, neg := g.encode(ctx, m)
	lat := nodes.EmptyLatentImage(ctx, "latent", nodes.EmptyLatentImageArgs{Width: g.Width, Height: g.Height, BatchSize: 1})
	return nodes.KSampler(ctx, "sampler", nodes.KSamplerArgs{
		Model: m.Model, Seed: g.Seed, Steps: g.Steps, Cfg: g.CFG,
		SamplerName: g.Sampler, Scheduler: g.Scheduler,
		Positive: pos, Negative: neg, LatentImage: lat.Latent,
	}).Latent
}

// Hires upscales a latent and refines it with a partial denoise.
func Hires(ctx *comfy.Context, m Model, in graph.Ref[nodes.LATENT], g Gen, scale, denoise float64) graph.Ref[nodes.LATENT] {
	pos, neg := g.encode(ctx, m)
	up := nodes.LatentUpscaleBy(ctx, "upscale", nodes.LatentUpscaleByArgs{Samples: in, UpscaleMethod: "bislerp", ScaleBy: scale})
	return nodes.KSampler(ctx, "sampler", nodes.KSamplerArgs{
		Model: m.Model, Seed: g.Seed, Steps: g.Steps / 2, Cfg: g.CFG,
		SamplerName: g.Sampler, Scheduler: g.Scheduler,
		Positive: pos, Negative: neg, LatentImage: up.Latent, Denoise: denoise,
	}).Latent
}

// Save decodes, optionally adds film grain (a custom node), and saves.
func Save(ctx *comfy.Context, m Model, lat graph.Ref[nodes.LATENT], prefix string, grain bool) {
	img := nodes.VAEDecode(ctx, "decode", nodes.VAEDecodeArgs{Samples: lat, Vae: m.VAE}).Image
	if grain {
		img = nodes.ImageFilmGrainWAS(ctx, "grain", nodes.ImageFilmGrainWASArgs{
			Image: img, Density: 0.3, Intensity: 0.15, Monochrome: graph.Bool(false),
		}).Image
	}
	nodes.SaveImage(ctx, "save", nodes.SaveImageArgs{Images: img, FilenamePrefix: prefix})
}
