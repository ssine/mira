import asyncio
import tempfile
from pathlib import Path
import unittest

from fastapi.testclient import TestClient
import httpx

from replica_relay import relay_app


class RelayTest(unittest.TestCase):
    def test_authentication_precedes_forwarding_and_routes_are_restricted(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'key'
            path.write_text('test-only-' + 'x' * 48)
            path.chmod(0o600)
            calls = []

            class Stream(httpx.AsyncByteStream):
                async def __aiter__(self): yield b'data: [DONE]\n\n'

            def backend(request):
                calls.append(request)
                self.assertNotIn('authorization', request.headers)
                return httpx.Response(200, stream=Stream(),
                                      headers={'content-type': 'text/event-stream'})

            app = relay_app(str(path), transport=httpx.MockTransport(backend))
            headers = {'Authorization': 'Bearer ' + path.read_text()}
            with TestClient(app) as client:
                self.assertEqual(401, client.get('/health').status_code)
                self.assertEqual(401, client.post('/recipe/v1/completions',
                    headers={'Authorization': 'Bearer wrong'}, content=b'invalid').status_code)
                self.assertFalse(calls)
                self.assertEqual(404, client.get('/metrics', headers=headers).status_code)
                self.assertEqual(404, client.post('/health', headers=headers).status_code)
                self.assertFalse(calls)
                self.assertEqual(200, client.get('/health', headers=headers).status_code)
                body = b'{"prompt":[1,2],"max_tokens":10}'
                response = client.post('/recipe/v1/completions', headers=headers, content=body)
                self.assertEqual(b'data: [DONE]\n\n', response.content)
                self.assertEqual(body, calls[-1].content)


class RelayAdmissionTest(unittest.IsolatedAsyncioTestCase):
    async def test_raw_inference_is_bounded_and_cancellation_releases_capacity(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'key';path.write_text('test-only-'+'x'*48);path.chmod(0o600)
            entered=asyncio.Event();calls=[]
            async def backend(request):
                if request.url.path=='/health': return httpx.Response(200,stream=httpx.ByteStream(b''))
                calls.append(request);entered.set()
                await asyncio.Event().wait()
            app=relay_app(str(path),transport=httpx.MockTransport(backend),max_inflight=1)
            async with app.router.lifespan_context(app), httpx.AsyncClient(
                transport=httpx.ASGITransport(app=app),base_url='http://relay',
                headers={'Authorization':'Bearer '+path.read_text()}) as client:
                active=asyncio.create_task(client.post('/recipe/v1/completions',content=b'{}'))
                await asyncio.wait_for(entered.wait(),2)
                self.assertEqual(429,(await client.post('/v1/completions',content=b'{}')).status_code)
                self.assertEqual(200,(await client.get('/health')).status_code)
                self.assertEqual(1,len(calls))
                active.cancel()
                with self.assertRaises(asyncio.CancelledError): await active
                self.assertEqual(0,app.state.admission.active)


if __name__ == '__main__': unittest.main()
