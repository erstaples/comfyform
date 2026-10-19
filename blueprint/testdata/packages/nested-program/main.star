def build(ctx, inputs, settings):
    base = ctx.use("test/hires", "base",
                   inputs = {"model": inputs.model, "positive": inputs.positive,
                             "negative": inputs.negative, "samples": inputs.samples},
                   settings = {"denoise": settings.denoise, "seed": settings.seed})
    return {"latent": base.latent}
