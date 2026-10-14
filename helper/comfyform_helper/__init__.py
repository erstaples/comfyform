"""comfyform helper: reports installed custom node packs so `comfy lock` can
pin them. Adds GET /comfyform/packs; registers no nodes."""

import asyncio

import folder_paths
from aiohttp import web
from server import PromptServer

from .packs import scan


@PromptServer.instance.routes.get("/comfyform/packs")
async def packs(request):
    data = await asyncio.to_thread(scan, folder_paths.get_folder_paths("custom_nodes"))
    return web.json_response(data)


NODE_CLASS_MAPPINGS = {}
NODE_DISPLAY_NAME_MAPPINGS = {}
