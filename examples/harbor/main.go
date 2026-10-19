// A fishing-harbor-at-dawn photo on comfy-2. The "model" config key picks
// Juggernaut XL ("sdxl", the default) or Flux.1 dev ("flux"). Stacks:
//
//	preview  768px, few steps, for iterating on the prompt
//	final    1024px, full steps, plus a 1.5x latent hires pass
//	sweep    one render per CFG value, sharing the checkpoint and prompt
//	flux     Flux.1 dev at 1024px
package main

import (
	"fmt"

	"github.com/erstaples/comfyform/comfy"
	"github.com/erstaples/comfyform/examples/harbor/nodes"
	"github.com/erstaples/comfyform/graph"
)

const checkpoint = "Juggernaut-XL_v9_RunDiffusionPhoto_v2.safetensors"

// Flux.1 dev ships as separate diffusion model, text encoder and VAE files.
const (
	fluxUNET = "flux1-dev-fp8-e4m3fn.safetensors"
	fluxCLIP = "clip_l.safetensors"
	fluxT5   = "t5xxl_fp16.safetensors"
	fluxVAE  = "ae.safetensors"
)

func main() {
	comfy.Run(func(ctx *comfy.Context) error {
		cfg := ctx.Config()
		gen := Gen{
			Prompt: "small fishing harbor at dawn, wooden boats moored at a stone pier, " +
				"mist over calm water, soft golden light, 35mm photo, high detail",
			Negative:  "blurry, lowres, text, watermark, oversaturated, cartoon",
			Seed:      cfg.IntOr("seed", 7),
			Steps:     cfg.IntOr("steps", 30),
			CFG:       cfg.FloatOr("cfg", 5),
			Sampler:   "dpmpp_2m_sde",
			Scheduler: "karras",
			Width:     cfg.IntOr("size", 1024),
			Height:    cfg.IntOr("size", 1024),
		}

		var m Model
		switch family := cfg.StringOr("model", "sdxl"); family {
		case "sdxl":
			m = SDXL(ctx.Child("base"))
		case "flux":
			m = Flux(ctx.Child("base"))
			// Flux is guidance-distilled: real CFG stays at 1 and the
			// guidance value goes into the conditioning instead.
			gen.CFG, gen.Guidance = 1, cfg.FloatOr("guidance", 3.5)
			gen.Sampler, gen.Scheduler = "euler", "simple"
		default:
			return fmt.Errorf("unknown model %q (want sdxl or flux)", family)
		}

		// sweep: one branch per CFG value, all in a single queued prompt
		if cfgs := cfg.Floats("sweep_cfg"); len(cfgs) > 0 {
			for _, c := range cfgs {
				g := gen
				g.CFG = c
				b := ctx.Child(fmt.Sprintf("cfg%03d", int(c*10)))
				Save(b, m, Txt2Img(b, m, g), fmt.Sprintf("harbor_cfg%03d", int(c*10)))
			}
			return nil
		}

		lat := Txt2Img(ctx.Child("gen"), m, gen)
		if cfg.Bool("hires") {
			lat = Hires(ctx.Child("hires"), m, lat, gen, 1.5, 0.4)
		}
		Save(ctx.Child("out"), m, lat, "harbor_"+ctx.Stack())
		return nil
	})
}

// Model is the MODEL/CLIP/VAE triple the stages pass around.
type Model struct {
	Model graph.Ref[nodes.MODEL]
	Clip  graph.Ref[nodes.CLIP]
	VAE   graph.Ref[nodes.VAE]
	Flux  bool
}

func SDXL(ctx *comfy.Context) Model {
	c := nodes.CheckpointLoaderSimple(ctx, "ckpt", nodes.CheckpointLoaderSimpleArgs{CkptName: checkpoint})
	return Model{Model: c.Model, Clip: c.Clip, VAE: c.VAE}
}

func Flux(ctx *comfy.Context) Model {
	return Model{
		Model: nodes.UNETLoader(ctx, "unet", nodes.UNETLoaderArgs{UnetName: fluxUNET}).Model,
		Clip:  nodes.DualCLIPLoader(ctx, "clip", nodes.DualCLIPLoaderArgs{ClipName1: fluxCLIP, ClipName2: fluxT5, Type: "flux"}).Clip,
		VAE:   nodes.VAELoader(ctx, "vae", nodes.VAELoaderArgs{VaeName: fluxVAE}).VAE,
		Flux:  true,
	}
}

type Gen struct {
	Prompt, Negative   string
	Seed, Steps        int64
	CFG, Guidance      float64 // Guidance is Flux-only
	Sampler, Scheduler string
	Width, Height      int64
}

func (g Gen) encode(ctx *comfy.Context, m Model) (pos, neg graph.Ref[nodes.CONDITIONING]) {
	pos = nodes.CLIPTextEncode(ctx, "pos", nodes.CLIPTextEncodeArgs{Text: g.Prompt, Clip: m.Clip}).Conditioning
	if m.Flux {
		// At CFG 1 the negative is ignored, so it only has to be well-formed.
		neg = nodes.ConditioningZeroOut(ctx, "neg", nodes.ConditioningZeroOutArgs{Conditioning: pos}).Conditioning
		pos = nodes.FluxGuidance(ctx, "guidance", nodes.FluxGuidanceArgs{Conditioning: pos, Guidance: g.Guidance}).Conditioning
		return
	}
	neg = nodes.CLIPTextEncode(ctx, "neg", nodes.CLIPTextEncodeArgs{Text: g.Negative, Clip: m.Clip}).Conditioning
	return
}

// emptyLatent picks the latent format: Flux uses 16 channels, SDXL 4.
func emptyLatent(ctx *comfy.Context, m Model, g Gen) graph.Ref[nodes.LATENT] {
	if m.Flux {
		return nodes.EmptySD3LatentImage(ctx, "latent", nodes.EmptySD3LatentImageArgs{Width: g.Width, Height: g.Height, BatchSize: 1}).Latent
	}
	return nodes.EmptyLatentImage(ctx, "latent", nodes.EmptyLatentImageArgs{Width: g.Width, Height: g.Height, BatchSize: 1}).Latent
}

// Txt2Img samples a fresh latent.
func Txt2Img(ctx *comfy.Context, m Model, g Gen) graph.Ref[nodes.LATENT] {
	pos, neg := g.encode(ctx, m)
	lat := emptyLatent(ctx, m, g)
	return nodes.KSampler(ctx, "sampler", nodes.KSamplerArgs{
		Model: m.Model, Seed: g.Seed, Steps: g.Steps, Cfg: g.CFG,
		SamplerName: g.Sampler, Scheduler: g.Scheduler,
		Positive: pos, Negative: neg, LatentImage: lat,
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

// Save decodes and writes the image.
func Save(ctx *comfy.Context, m Model, lat graph.Ref[nodes.LATENT], prefix string) {
	img := nodes.VAEDecode(ctx, "decode", nodes.VAEDecodeArgs{Samples: lat, Vae: m.VAE}).Image
	nodes.SaveImage(ctx, "save", nodes.SaveImageArgs{Images: img, FilenamePrefix: prefix})
}
