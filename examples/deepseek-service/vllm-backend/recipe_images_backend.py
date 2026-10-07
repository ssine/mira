"""vLLM endpoint plugin: Recipe token IDs + preprocessed WebP images.

Install only in the inference environment and explicitly opt in with
VLLM_PLUGINS=deepseek_recipe_images. Keep the engine on loopback; only the
authenticated Recipe frontend is exposed by the Mira site.
"""
import base64
import binascii
import io
from uuid import uuid4

from anyio import CancelScope
from fastapi import Request
from fastapi.responses import JSONResponse, StreamingResponse
from PIL import Image, UnidentifiedImageError
from pydantic import Field, ValidationError, model_validator
from starlette.concurrency import run_in_threadpool

from vllm.entrypoints.openai.completion.protocol import CompletionRequest
from vllm.entrypoints.openai.completion.serving import OpenAIServingCompletion
from vllm.entrypoints.serve.engine.protocol import ErrorResponse


class ImageCompletionRequest(CompletionRequest):
    images: list[str] = Field(default_factory=list, max_length=600)
    expected_prompt_tokens: int = Field(gt=0)

    @model_validator(mode="after")
    def raw_single_stream(self):
        if (not self.prompt or not isinstance(self.prompt, list)
                or any(type(token) is not int for token in self.prompt)
                or self.prompt.count(129264) != len(self.images)
                or self.n != 1 or not self.stream or not self.return_token_ids
                or self.add_special_tokens or self.prompt_embeds is not None
                or self.truncate_prompt_tokens is not None or self.use_beam_search):
            raise ValueError("Expected one untruncated raw-token image prompt and token-ID stream.")
        return self


def decode_images(encoded):
    images = []
    for value in encoded:
        data = base64.b64decode(value, validate=True)
        # The public frontend has already applied Recipe's decode/resize rules.
        # This private boundary accepts those results only, with no URL fetching.
        with Image.open(io.BytesIO(data)) as image:
            if image.format != "WEBP" or max(image.size) > 8192 or image.width * image.height > 4_000_000:
                raise ValueError("Expected a preprocessed WebP image.")
            images.append(image.convert("RGB"))
    return images


class ImageCompletions(OpenAIServingCompletion):
    def __init__(self, original):
        # Reuse initialized engine, renderer and immutable serving configuration.
        # Per-request images stay on the request, never on this shared instance.
        self.__dict__.update(vars(original))
        # These are API-serving options, independent of engine/GPU settings.
        self.enable_prompt_tokens_details = True
        self.enable_per_request_metrics = True

    async def render_completion_request(self, request):
        error = await self._check_model(request)
        if error is not None:
            return error
        self._preflight(1)
        prompt = {"prompt_token_ids": request.prompt}
        if request.images:
            images = await run_in_threadpool(decode_images, request.images)
            prompt["multi_modal_data"] = {"image": images}
        inputs = await self.online_renderer.preprocess_cmpl(request, [prompt])
        if len(inputs) != 1 or self._extract_prompt_len(inputs[0]) != request.expected_prompt_tokens:
            raise ValueError("Recipe and vLLM image token counts disagree.")
        return inputs


class ClosingStream(StreamingResponse):
    async def __call__(self, scope, receive, send):
        try:
            await super().__call__(scope, receive, send)
        finally:
            with CancelScope(shield=True):
                await self.body_iterator.aclose()


class RecipeImagesPlugin:
    name = "deepseek_recipe_images"
    required_tasks = ("generate",)

    def attach_router(self, app):
        app.add_api_route("/recipe/v1/completions", self.completions, methods=["POST"])

    async def init_state(self, engine_client, state, args):
        if args.host not in ("127.0.0.1", "::1", "localhost"):
            raise ValueError("The Recipe image backend must listen on loopback.")
        state.recipe_image_completions = ImageCompletions(state.openai_serving_completion)

    async def completions(self, request: Request):
        serving = request.app.state.recipe_image_completions
        body = bytearray()
        async for chunk in request.stream():
            if len(body) + len(chunk) > 64 * 1024 * 1024:
                return JSONResponse({"error": "Image backend request too large."}, status_code=413)
            body.extend(chunk)
        try:
            parsed = ImageCompletionRequest.model_validate_json(body)
            # Server-owned ID makes disconnect cleanup independent of headers.
            parsed.request_id = uuid4().hex
            result = await serving.create_completion(parsed)
        except (ValueError, ValidationError, UnidentifiedImageError, binascii.Error):
            return JSONResponse({"error": "Invalid image completion request."}, status_code=400)
        if isinstance(result, ErrorResponse):
            return JSONResponse(result.model_dump(), status_code=result.error.code)

        async def output():
            try:
                async for chunk in result:
                    yield chunk
            finally:
                with CancelScope(shield=True):
                    await result.aclose()
                    await serving.engine_client.abort(f"cmpl-{parsed.request_id}-0")

        return ClosingStream(output(), media_type="text/event-stream")
