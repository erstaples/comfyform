// Imported by comfy import. Rename nodes freely; ids only need to be unique.
package main

import (
	"github.com/erstaples/comfyform/comfy"
	"github.com/erstaples/comfyform/examples/imported/nodes"
	"github.com/erstaples/comfyform/graph"
)

func main() {
	comfy.Run(func(ctx *comfy.Context) error {
		checkpointloadersimple4 := nodes.CheckpointLoaderSimple(ctx, "checkpointloadersimple_4", nodes.CheckpointLoaderSimpleArgs{
			CkptName: "sd15.safetensors",
		})
		emptylatentimage5 := nodes.EmptyLatentImage(ctx, "emptylatentimage_5", nodes.EmptyLatentImageArgs{
			Width:     512,
			Height:    512,
			BatchSize: 1,
		})
		// Seed (rgthree) is not in the schema; emitted as a raw node.
		seedrgthree12 := ctx.Node("seedrgthree_12", "Seed (rgthree)", graph.Inputs{
			"seed": 1234,
		}, nil)
		loraloader10 := nodes.LoraLoader(ctx, "loraloader_10", nodes.LoraLoaderArgs{
			Model:         checkpointloadersimple4.Model,
			Clip:          checkpointloadersimple4.Clip,
			LoraName:      "film_grain.safetensors",
			StrengthModel: 0.9,
			Raw: graph.Inputs{
				"strength_clip": 0,
			},
		})
		positive := nodes.CLIPTextEncode(ctx, "positive", nodes.CLIPTextEncodeArgs{
			Text: "beautiful scenery nature glass bottle landscape, purple galaxy bottle",
			Clip: loraloader10.Clip,
		})
		negative := nodes.CLIPTextEncode(ctx, "negative", nodes.CLIPTextEncodeArgs{
			Clip: loraloader10.Clip,
		})
		ksampler3 := nodes.KSampler(ctx, "ksampler_3", nodes.KSamplerArgs{
			Model:       loraloader10.Model,
			Steps:       20,
			Cfg:         8,
			SamplerName: "euler",
			Scheduler:   "normal",
			Positive:    positive.Conditioning,
			Negative:    negative.Conditioning,
			LatentImage: emptylatentimage5.Latent,
			Denoise:     1,
			Raw: graph.Inputs{
				"seed": graph.Link(seedrgthree12, 0),
			},
		})
		vaedecode8 := nodes.VAEDecode(ctx, "vaedecode_8", nodes.VAEDecodeArgs{
			Samples: ksampler3.Latent,
			Vae:     checkpointloadersimple4.VAE,
		})
		nodes.SaveImage(ctx, "saveimage_9", nodes.SaveImageArgs{
			Images:         vaedecode8.Image,
			FilenamePrefix: "ComfyUI",
		})
		return nil
	})
}
