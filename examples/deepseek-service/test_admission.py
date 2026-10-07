import asyncio
import unittest

from admission import Admission, QueueFull, QueueTimeout
from service import Settings, RequestError, stream_while_waiting


class AdmissionTest(unittest.IsolatedAsyncioTestCase):
    async def test_bounded_fifo_and_no_new_request_overtaking(self):
        gate = Admission(1, 2)
        first, second, third = gate.reserve(), gate.reserve(), gate.reserve()
        with self.assertRaises(QueueFull):
            gate.reserve()
        self.assertFalse(second.ready.done())
        first.release()
        self.assertTrue(second.ready.done())
        self.assertFalse(third.ready.done())
        fourth = gate.reserve()
        second.release()
        self.assertTrue(third.ready.done())
        self.assertFalse(fourth.ready.done())
        third.release()
        fourth.release()
        self.assertEqual((0, 0), (gate.active, len(gate.waiting)))

    async def test_cancellation_and_grant_race_release_capacity_once(self):
        gate = Admission(1, 3)
        first, cancelled, survivor = gate.reserve(), gate.reserve(), gate.reserve()
        waiter = asyncio.create_task(cancelled.wait(10))
        await asyncio.sleep(0)
        waiter.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await waiter
        cancelled.release()
        first.release()
        # Cancellation can arrive after a ticket was granted but before inference.
        survivor.release()
        survivor.release()
        self.assertEqual((0, 0), (gate.active, len(gate.waiting)))

    async def test_timeout_removes_waiter_without_cancelling_an_active_request(self):
        gate = Admission(1, 1)
        active, queued = gate.reserve(), gate.reserve()
        with self.assertRaises(QueueTimeout):
            await queued.wait(0.01)
        queued.release()
        self.assertEqual(1, gate.active)
        replacement = gate.reserve()
        active.release()
        self.assertGreaterEqual(await replacement.wait(1), 0)
        replacement.release()

    async def test_pause_drains_active_work_and_resume_keeps_order(self):
        gate = Admission(1, 2)
        active = gate.reserve()
        gate.set_ready(False)
        queued = gate.reserve()
        active.release()
        self.assertFalse(queued.ready.done())
        gate.set_ready(True)
        await queued.wait(1)
        queued.release()

    async def test_zero_queue_allows_free_execution_slots(self):
        gate = Admission(1, 0)
        active = gate.reserve()
        with self.assertRaises(QueueFull):
            gate.reserve()
        active.release()
        gate.set_ready(False)
        with self.assertRaises(QueueFull):
            gate.reserve()

    async def test_sse_keepalive_and_cancellation_close_pending_output(self):
        closed = asyncio.Event()

        async def blocked():
            try:
                await asyncio.Event().wait()
                yield "unused"
            finally:
                closed.set()

        stream = stream_while_waiting(blocked(), interval=0.01)
        self.assertEqual('event: ping\ndata: {"type":"ping"}\n\n', await anext(stream))
        await stream.aclose()
        self.assertTrue(closed.is_set())

    async def test_queued_stream_errors_never_emit_completion(self):
        async def failed():
            raise RequestError("Queue timeout", 429, "rate_limit_error")
            yield "unused"

        parts = [part async for part in stream_while_waiting(failed(), interval=0.01)]
        self.assertIn('"code": "rate_limit_error"', "".join(parts))
        self.assertNotIn("response.completed", "".join(parts))

    def test_invalid_capacity_configuration_is_rejected(self):
        for options in ({"max_inflight": 0}, {"max_queued": -1},
                        {"queue_timeout": 0}, {"queue_timeout": float("inf")},
                        {"backend_ready_file": "relative"}):
            with self.assertRaises(ValueError):
                Settings("tokenizer", "key", **options)
