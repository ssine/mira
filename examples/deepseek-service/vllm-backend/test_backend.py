"""Run in the pinned vLLM environment; no model/GPU allocation is needed."""
import asyncio
import base64
import io
from types import SimpleNamespace
import unittest
from unittest.mock import AsyncMock, Mock

from fastapi import FastAPI
from fastapi.testclient import TestClient
from PIL import Image

from recipe_images_backend import ImageCompletionRequest, ImageCompletions, RecipeImagesPlugin, decode_images


def picture(color="red"):
    stream = io.BytesIO()
    Image.new("RGB", (546, 546), color).save(stream, format="WEBP")
    return base64.b64encode(stream.getvalue()).decode()


def payload(**overrides):
    return dict(model="test", prompt=[0, 129264, 42], images=[picture()],
                expected_prompt_tokens=186, stream=True, return_token_ids=True,
                add_special_tokens=False, **overrides)


class BackendTest(unittest.IsolatedAsyncioTestCase):
    async def test_raw_tokens_and_images_reach_renderer_and_count_is_checked(self):
        seen = []

        async def render(request, prompts):
            await asyncio.sleep(0)
            seen.append(prompts[0])
            return [{"prompt_token_ids": [0] * 186}]

        original = SimpleNamespace(
            _check_model=AsyncMock(return_value=None), _preflight=Mock(),
            _extract_prompt_len=lambda prompt: len(prompt["prompt_token_ids"]),
            online_renderer=SimpleNamespace(preprocess_cmpl=render),
        )
        serving = ImageCompletions(original)
        first = ImageCompletionRequest.model_validate(payload())
        second_body = payload()
        second_body["images"] = [picture("blue")]
        second = ImageCompletionRequest.model_validate(second_body)
        await asyncio.gather(serving.render_completion_request(first), serving.render_completion_request(second))
        self.assertEqual([0, 129264, 42], seen[0]["prompt_token_ids"])
        colors = [prompt["multi_modal_data"]["image"][0].getpixel((100, 100)) for prompt in seen]
        self.assertGreater(colors[0][0], colors[0][2])
        self.assertGreater(colors[1][2], colors[1][0])
        first.expected_prompt_tokens = 185
        with self.assertRaisesRegex(ValueError, "token counts disagree"):
            await serving.render_completion_request(first)

    async def test_truncation_mismatched_placeholders_and_non_images_rejected(self):
        for changes in ({"prompt": [0]}, {"n": 2}, {"truncate_prompt_tokens": 100}, {"prompt": "text"}):
            data = payload()
            data.update(changes)
            with self.assertRaises(ValueError):
                ImageCompletionRequest.model_validate(data)
        with self.assertRaises(Exception):
            decode_images([base64.b64encode(b"not an image").decode()])

    async def test_route_stream_closes_and_aborts_owned_request(self):
        closed = []

        async def generate(request):
            async def chunks():
                try:
                    yield 'data: {"choices":[]}\n\n'
                    yield "data: [DONE]\n\n"
                finally:
                    closed.append(True)
            return chunks()

        engine = SimpleNamespace(abort=AsyncMock())
        app = FastAPI()
        RecipeImagesPlugin().attach_router(app)
        app.state.recipe_image_completions = SimpleNamespace(create_completion=generate, engine_client=engine)
        with TestClient(app) as client:
            response = client.post("/recipe/v1/completions", json=payload())
        self.assertEqual(200, response.status_code)
        self.assertIn("[DONE]", response.text)
        self.assertEqual([True], closed)
        engine.abort.assert_awaited_once()
        self.assertRegex(engine.abort.call_args.args[0], r"^cmpl-[a-f0-9]{32}-0$")


if __name__ == "__main__":
    unittest.main()
