// A cabin-at-dusk workflow with three stacks:
//
//	preview  fast, small, no hires
//	final    full steps, hires pass, film grain
//	sweep    one render per LoRA weight, sharing the checkpoint and prompt
package main

import (
	"fmt"

	"github.com/erstaples/comfyform/comfy"
	"github.com/erstaples/comfyform/examples/cabin/stages"
)

func main() {
	comfy.Run(func(ctx *comfy.Context) error {
		cfg := ctx.Config()

		gen := stages.Gen{
			Prompt:    "small cabin in a snowy mountain valley at dusk, warm window light, hand-painted watercolor",
			Negative:  "blurry, lowres, text, watermark",
			Seed:      cfg.IntOr("seed", 42),
			Steps:     cfg.IntOr("steps", 30),
			CFG:       6.5,
			Sampler:   "dpmpp_2m",
			Scheduler: "karras",
			Width:     cfg.IntOr("size", 1024),
			Height:    cfg.IntOr("size", 1024),
		}

		base := stages.Checkpoint(ctx.Child("base"), "sdxl.safetensors")

		// sweep: one branch per LoRA weight, all in a single queued prompt
		if weights := cfg.Floats("sweep_lora"); len(weights) > 0 {
			for _, w := range weights {
				b := ctx.Child(fmt.Sprintf("w%03d", int(w*100)))
				m := stages.WithLoras(b, base, stages.Lora{Name: "ghibli_wc.safetensors", Weight: w})
				stages.Save(b, m, stages.Txt2Img(b, m, gen), fmt.Sprintf("cabin_lora%03d", int(w*100)), false)
			}
			return nil
		}

		m := stages.WithLoras(ctx.Child("base"), base,
			stages.Lora{Name: "ghibli_wc.safetensors", Weight: cfg.FloatOr("lora_weight", 0.8)})

		lat := stages.Txt2Img(ctx.Child("gen"), m, gen)
		if cfg.Bool("hires") {
			lat = stages.Hires(ctx.Child("hires"), m, lat, gen, 1.5, 0.45)
		}
		stages.Save(ctx.Child("out"), m, lat, "cabin_"+ctx.Stack(), cfg.Bool("grain"))
		return nil
	})
}
