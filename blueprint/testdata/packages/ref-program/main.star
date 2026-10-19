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
