"""Standalone, authenticated Recipe frontend for a loopback vLLM engine.

This application is not part of Mira's proxy or Node runtime. Recipe owns all
prompt rendering and protocol parsing; vLLM receives and returns raw token IDs.
"""
# Conversion/streaming flow adapted from DeepSeek Recipe's server-py example.
# See THIRD_PARTY_NOTICES.md for its MIT license.

import argparse
import asyncio
import base64
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from dataclasses import dataclass
import hmac
import json
import os
import stat
from time import time
from uuid import uuid4

from anyio import CancelScope
from deepseek_recipe import (
    ChatCompletionRequest, ChatCompletionResponse, ConversionError, EOS_TOKEN,
    ConversionOptions, DeepseekV41Encoding, InferenceChunk,
    InferenceFinishReason, PromptUsage, ResponsesRequest, ResponsesResponse,
    StreamProcessor, Tokenizer, WebSearchBehavior,
    IMAGE_SPECIAL_TOKEN, ImageResolver, ImageQuota, ImageError, CalcResizeError,
    OpenCvImagePreprocessor, ReqwestImageFetcher,
)
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, Response, StreamingResponse
import httpx
from starlette.concurrency import run_in_threadpool


PROTOCOLS = {
    "responses": (ResponsesRequest, ResponsesResponse),
    "chat_completions": (ChatCompletionRequest, ChatCompletionResponse),
}


@dataclass(frozen=True)
class Settings:
    tokenizer: str
    key_file: str
    backend: str = "http://127.0.0.1:8000"
    model: str = "DeepSeek-V4.1-Flash"
    context_tokens: int = 294912
    default_output_tokens: int = 32768
    default_effort: str = "max"
    max_request_bytes: int = 32 * 1024 * 1024
    max_inflight: int = 4


class RequestError(Exception):
    def __init__(self, message: str, status: int = 400, code: str = "invalid_request_error"):
        self.message, self.status, self.code = message, status, code


def error_response(message: str, status: int, code: str) -> JSONResponse:
    return JSONResponse({"error": {"message": message, "type": code, "code": code}}, status_code=status)


def read_key(path: str) -> bytes:
    with open(path, "rb") as handle:
        info = os.fstat(handle.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077:
            raise ValueError("API key file must be a private regular file (mode 0600)")
        key = handle.read(4097).strip()
    if not 32 <= len(key) <= 4096 or any(c < 33 or c > 126 for c in key):
        raise ValueError("API key must contain 32 to 4096 printable non-space ASCII bytes")
    return key


class AuthAndCapacity:
    """Authenticate every path before reading a body, with a bounded live set."""
    def __init__(self, app, key: bytes, limit: int):
        self.app, self.key, self.limit, self.active = app, key, limit, 0

    async def __call__(self, scope, receive, send):
        if scope["type"] != "http":
            if scope["type"] == "websocket":
                await send({"type": "websocket.close", "code": 1008})
                return
            await self.app(scope, receive, send)
            return
        headers = [v for k, v in scope["headers"] if k.lower() == b"authorization"]
        parts = headers[0].split() if len(headers) == 1 else []
        if len(parts) != 2 or parts[0].lower() != b"bearer" or not hmac.compare_digest(parts[1], self.key):
            response = error_response("A valid Bearer API key is required.", 401, "invalid_api_key")
            response.headers["WWW-Authenticate"] = "Bearer"
            await response(scope, receive, send)
            return
        # No await between checking and incrementing: one Uvicorn event loop.
        if self.active >= self.limit:
            await error_response("Service is busy; retry later.", 429, "rate_limit_error")(scope, receive, send)
            return
        self.active += 1
        try:
            await self.app(scope, receive, send)
        finally:
            self.active -= 1


@dataclass
class Prepared:
    protocol: str
    converted: object
    tokens: list[int]
    max_tokens: int
    include_usage: bool
    custom_tools: frozenset[str]
    images: list[str]
    prompt_tokens: int


def prepare(protocol: str, body: bytes, settings: Settings, tokenizer: Tokenizer) -> Prepared:
    try:
        payload = json.loads(body)
    except (ValueError, UnicodeDecodeError) as exc:
        raise RequestError("Body must be a JSON object.") from exc
    if not isinstance(payload, dict):
        raise RequestError("Body must be a JSON object.")
    if payload.get("model") != settings.model:
        raise RequestError("Unknown model.", 404, "model_not_found")
    if protocol == "responses" and any(payload.get(k) for k in ("previous_response_id", "conversation", "store", "background")):
        raise RequestError("This service is stateless; send complete input history with store=false.")
    # Keep the existing deployment's default strength 100, while explicit
    # low/high/max use Recipe's official V4.1 mapping (50/75/100).
    if protocol == "responses":
        reasoning = payload.get("reasoning")
        if reasoning is None:
            payload["reasoning"] = {"effort": settings.default_effort}
        elif isinstance(reasoning, dict) and reasoning.get("effort") is None:
            payload["reasoning"] = {**reasoning, "effort": settings.default_effort}
    elif payload.get("reasoning_effort") is None:
        payload["reasoning_effort"] = settings.default_effort
    request_type, _ = PROTOCOLS[protocol]
    try:
        request = request_type(payload)
        include_usage = request.include_usage() if protocol == "chat_completions" else False
        custom_tools = frozenset(request.custom_tool_names()) if protocol == "responses" else frozenset()
        converted = request.convert(ConversionOptions(
            default_thinking_mode=True,
            responses_web_search=WebSearchBehavior.Reject,
        ))
        rendered = DeepseekV41Encoding().render_conversation(converted.conversation)
        images = []
        adjustment = 0
        prompt = rendered.prompt
        if rendered.image_sources:
            # Codex sends inline images. Do not turn application authentication
            # into permission to fetch arbitrary URLs from the engine network.
            if any(source.kind == "url" for source in rendered.image_sources):
                raise RequestError("Supply images inline as data URLs; external image URLs are unsupported.")
            multimodal = ImageResolver(ReqwestImageFetcher(), OpenCvImagePreprocessor()).resolve(
                rendered.image_sources, ImageQuota()
            )
            images = [base64.b64encode(image.data).decode("ascii") for image in multimodal.images]
            adjustment = multimodal.image_token_adjustment()
            # The released checkpoint spells Recipe's image sentinel differently.
            # Preserve every other token; vLLM expands each sentinel using the
            # matching vision processor and out-of-band image embeddings.
            checkpoint_marker = "<｜deepseek_image｜>"
            if tokenizer.encode(checkpoint_marker) != [129264]:
                raise RequestError("The checkpoint image token is incompatible.", 500, "configuration_error")
            prompt = prompt.replace(IMAGE_SPECIAL_TOKEN, checkpoint_marker)
        tokens = tokenizer.encode(prompt)
        if tokens.count(129264) != len(images):
            raise RequestError("Image placeholders must match the attached images.")
    except (ImageError, CalcResizeError) as exc:
        raise RequestError("Invalid image or image preprocessing limit exceeded.") from exc
    except ValueError as exc:
        raise RequestError(str(exc)) from exc
    prompt_tokens = len(tokens) + adjustment
    remaining = settings.context_tokens - prompt_tokens
    max_tokens = converted.inference_options.max_tokens
    if max_tokens is None:
        max_tokens = min(settings.default_output_tokens, remaining)
    if max_tokens < 1 or max_tokens > remaining:
        raise RequestError("Input and requested output exceed the model context window.", 400, "context_length_exceeded")
    return Prepared(protocol, converted, tokens, max_tokens, include_usage, custom_tools, images, prompt_tokens)


async def inference(client: httpx.AsyncClient, prepared: Prepared, settings: Settings, eos_token_id: int) -> AsyncIterator[InferenceChunk]:
    options = prepared.converted.inference_options
    payload = {
        "model": settings.model, "prompt": prepared.tokens,
        "max_tokens": prepared.max_tokens,
        "temperature": options.temperature if options.temperature is not None else 1.0,
        "top_p": options.top_p if options.top_p is not None else 0.95,
        "stream": True,
        "stream_options": {"include_usage": True, "continuous_usage_stats": True},
        "return_token_ids": True, "add_special_tokens": False,
        "skip_special_tokens": False,
    }
    if options.thinking_budget_tokens is not None:
        payload["thinking_token_budget"] = options.thinking_budget_tokens
    path = "/v1/completions"
    if prepared.images:
        path = "/recipe/v1/completions"
        payload.update(images=prepared.images, expected_prompt_tokens=prepared.prompt_tokens)
    response = None
    ready = False
    finished = False
    try:
        request = client.build_request("POST", path, json=payload)
        response = await client.send(request, stream=True)
        if response.status_code != 200:
            # Never forward engine internals, request bodies or credentials.
            raise RequestError(f"Inference backend returned HTTP {response.status_code}.", 502, "backend_error")
        async for line in response.aiter_lines():
            if not line.startswith("data: "):
                continue
            data = line[6:]
            if data == "[DONE]":
                break
            event = json.loads(data)
            if event.get("error"):
                raise RequestError("Inference backend reported a stream error.", 502, "backend_error")
            if not ready:
                usage = event.get("usage") or {}
                details = usage.get("prompt_tokens_details") or {}
                yield InferenceChunk.ready(prompt_usage=PromptUsage(
                    prompt_tokens=usage.get("prompt_tokens", prepared.prompt_tokens),
                    prompt_cache_hit_tokens=details.get("cached_tokens", 0) or 0,
                ))
                ready = True
            for choice in event.get("choices", []):
                ids = choice.get("token_ids")
                if ids is None and choice.get("text"):
                    raise RequestError("Inference backend must return token IDs.", 502, "backend_error")
                for token in ids or []:
                    # vLLM includes the sampled EOS ID even when its text is
                    # suppressed. Recipe expects the backend finish signal;
                    # retain EOS in usage without leaking it into answer text.
                    yield InferenceChunk.text("", content_tokens=1) if token == eos_token_id else InferenceChunk.token(token)
                reason = choice.get("finish_reason")
                if reason:
                    if reason not in ("length", "stop"):
                        raise RequestError("Unsupported backend finish reason.", 502, "backend_error")
                    finished = True
                    yield InferenceChunk.finish(
                        InferenceFinishReason.Length if reason == "length" else InferenceFinishReason.Stop
                    )
                    return
        if not finished:
            raise RequestError("Inference stream ended before completion.", 502, "backend_error")
    except (httpx.HTTPError, ValueError) as exc:
        raise RequestError("Inference backend connection failed.", 502, "backend_error") from exc
    finally:
        if response is not None:
            with CancelScope(shield=True):
                await response.aclose()


def sse(event: str | None, data: str) -> str:
    return (f"event: {event}\n" if event else "") + f"data: {data}\n\n"


async def response_body(prepared: Prepared, client: httpx.AsyncClient, settings: Settings, tokenizer: Tokenizer):
    request_type, response_type = PROTOCOLS[prepared.protocol]
    response_id = "resp_" + uuid4().hex
    generator = request_type.chunk_generator(prepared.converted, response_id, settings.model)
    if prepared.protocol == "responses":
        generator = generator.with_custom_tool_names(prepared.custom_tools)
    else:
        generator = generator.with_include_usage(prepared.include_usage)
    processor = StreamProcessor(generator, prepared.converted.parsing_options, tokenizer)
    accumulated = None if prepared.converted.stream else response_type(response_id, settings.model, int(time()), 0, 0)
    backend = inference(client, prepared, settings, tokenizer.encode(EOS_TOKEN)[0])
    try:
        async for chunk in backend:
            for event in processor.push(chunk):
                if prepared.converted.stream:
                    yield sse(response_type.chunk_event_type(event), event.to_json())
                else:
                    accumulated.append(event)
            if processor.finished:
                break
        for event in processor.finish():
            if prepared.converted.stream:
                yield sse(response_type.chunk_event_type(event), event.to_json())
            else:
                accumulated.append(event)
        if accumulated is not None:
            yield accumulated.to_json()
        elif (done := response_type.done_message()) is not None:
            yield sse(None, done)
    finally:
        with CancelScope(shield=True):
            await backend.aclose()
        processor.close()


class ClosingStreamingResponse(StreamingResponse):
    async def __call__(self, scope, receive, send):
        try:
            await super().__call__(scope, receive, send)
        finally:
            with CancelScope(shield=True):
                await self.body_iterator.aclose()


async def stream_after_first(first, output):
    try:
        yield first
        async for part in output:
            yield part
    except RequestError as exc:
        # Never emit a false completed event on an interrupted upstream stream.
        yield sse("error", json.dumps({"type": "error", "code": exc.code, "message": exc.message}))
    finally:
        with CancelScope(shield=True):
            await output.aclose()


async def first_or_disconnect(request: Request, output):
    """Cancel inference when a non-streaming caller closes its connection too."""
    async def disconnected():
        while True:
            if (await request.receive())["type"] == "http.disconnect":
                return
    first = asyncio.create_task(anext(output))
    watcher = asyncio.create_task(disconnected())
    try:
        done, _ = await asyncio.wait((first, watcher), return_when=asyncio.FIRST_COMPLETED)
        if first in done:
            return first.result()
        raise RequestError("Client disconnected.", 499, "client_disconnected")
    finally:
        for task in (first, watcher):
            if not task.done():
                task.cancel()
        await asyncio.gather(first, watcher, return_exceptions=True)


def create_app(settings: Settings, *, transport=None) -> FastAPI:
    key = read_key(settings.key_file)
    tokenizer = Tokenizer.from_file(settings.tokenizer)

    @asynccontextmanager
    async def lifespan(app):
        async with httpx.AsyncClient(
            base_url=settings.backend, trust_env=False, transport=transport,
            timeout=httpx.Timeout(connect=5, read=600, write=30, pool=5),
            limits=httpx.Limits(max_connections=settings.max_inflight, max_keepalive_connections=settings.max_inflight),
        ) as client:
            app.state.client = client
            yield

    app = FastAPI(lifespan=lifespan, docs_url=None, redoc_url=None, openapi_url=None)
    app.add_middleware(AuthAndCapacity, key=key, limit=settings.max_inflight)

    @app.exception_handler(RequestError)
    async def request_error(_request, exc):
        return error_response(exc.message, exc.status, exc.code)

    @app.exception_handler(ConversionError)
    async def conversion_error(_request, exc):
        return Response(content=exc.body, status_code=exc.status_code, media_type="application/json")

    @app.get("/health")
    async def health(request: Request):
        try:
            response = await request.app.state.client.get("/health", timeout=5)
            if response.status_code == 200:
                return {"status": "ok"}
        except httpx.HTTPError:
            pass
        raise RequestError("Inference backend unavailable.", 503, "backend_unavailable")

    @app.get("/v1/models")
    async def models():
        return {"object": "list", "data": [{"id": settings.model, "object": "model", "owned_by": "local", "context_window": settings.context_tokens}]}

    def handler(protocol):
        async def endpoint(request: Request):
            body = bytearray()
            async for chunk in request.stream():
                if len(body) + len(chunk) > settings.max_request_bytes:
                    raise RequestError("Request body is too large.", 413, "request_too_large")
                body.extend(chunk)
            prepared = await run_in_threadpool(prepare, protocol, bytes(body), settings, tokenizer)
            output = response_body(prepared, request.app.state.client, settings, tokenizer)
            try:
                first = await first_or_disconnect(request, output)
            except BaseException:
                with CancelScope(shield=True):
                    await output.aclose()
                raise
            if prepared.converted.stream:
                return ClosingStreamingResponse(stream_after_first(first, output), media_type="text/event-stream",
                    headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"})
            try:
                return Response(first, media_type="application/json")
            finally:
                await output.aclose()
        return endpoint

    app.add_api_route("/v1/responses", handler("responses"), methods=["POST"])
    app.add_api_route("/v1/chat/completions", handler("chat_completions"), methods=["POST"])
    return app


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tokenizer", required=True)
    parser.add_argument("--key-file", required=True)
    parser.add_argument("--backend", default="http://127.0.0.1:8000")
    parser.add_argument("--port", type=int, default=8001)
    args = parser.parse_args()
    import uvicorn
    app = create_app(Settings(tokenizer=args.tokenizer, key_file=args.key_file, backend=args.backend))
    uvicorn.run(app, host="127.0.0.1", port=args.port, access_log=False, server_header=False)


if __name__ == "__main__":
    main()
