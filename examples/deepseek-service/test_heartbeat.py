"""First-event heartbeats and cancellation preserve the Responses lifecycle."""
import asyncio
import json
import unittest

from service import RequestError, stream_after_first, stream_while_waiting


class HeartbeatTest(unittest.IsolatedAsyncioTestCase):
    async def test_delayed_first_event_sends_parseable_pings_then_original_output(self):
        async def delayed():
            await asyncio.sleep(.08)
            yield 'event: response.created\ndata: {"type":"response.created"}\n\n'
            yield 'event: response.completed\ndata: {"type":"response.completed"}\n\n'

        parts = [part async for part in stream_while_waiting(delayed(), interval=.01)]
        events = [json.loads(part.split('data: ', 1)[1]) for part in parts]
        self.assertGreaterEqual(len(events), 3)
        self.assertTrue(all(event == {'type': 'ping'} for event in events[:-2]))
        self.assertEqual(['response.created', 'response.completed'], [event['type'] for event in events[-2:]])

    async def test_error_before_first_heartbeat_retains_http_error(self):
        async def failed():
            raise RequestError('Backend unavailable.', 502, 'backend_unavailable')
            yield

        with self.assertRaises(RequestError) as caught:
            await anext(stream_while_waiting(failed(), errors_as_events=False))
        self.assertEqual(502, caught.exception.status)

    async def test_error_after_heartbeat_emits_error_without_false_completion(self):
        async def failed():
            await asyncio.sleep(.05)
            raise RequestError('Queue expired.', 429, 'rate_limit_error')
            yield

        waiting = stream_while_waiting(failed(), interval=.01, errors_as_events=False)
        first = await anext(waiting)
        parts = [part async for part in stream_after_first(first, waiting)]
        self.assertTrue(parts[0].startswith('event: ping\n'))
        self.assertIn('rate_limit_error', parts[-1])
        self.assertNotIn('response.completed', ''.join(parts))

    async def test_disconnect_after_heartbeat_closes_pending_first_event(self):
        closed = asyncio.Event()

        async def delayed():
            try:
                await asyncio.sleep(30)
                yield 'unreachable'
            finally:
                closed.set()

        output = stream_while_waiting(delayed(), interval=.01)
        self.assertTrue((await anext(output)).startswith('event: ping\n'))
        await output.aclose()
        self.assertTrue(closed.is_set())


if __name__ == '__main__':
    unittest.main()
