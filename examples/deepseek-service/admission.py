"""Bounded FIFO inference admission, owned by one ASGI event loop."""
import asyncio
from collections import deque
from time import perf_counter


class QueueFull(Exception):
    pass


class QueueTimeout(Exception):
    pass


class Ticket:
    def __init__(self, owner):
        self.owner = owner
        self.ready = asyncio.get_running_loop().create_future()
        self.created = perf_counter()
        self.granted = None
        self.closed = False

    async def wait(self, timeout):
        try:
            remaining = max(0, self.created + timeout - perf_counter())
            await asyncio.wait_for(asyncio.shield(self.ready), remaining)
        except TimeoutError as exc:
            raise QueueTimeout from exc
        if self.granted - self.created > timeout:
            raise QueueTimeout
        return (self.granted - self.created) * 1000

    def release(self):
        if self.closed:
            return
        self.closed = True
        if self.granted is None:
            self.owner.waiting.remove(self)
            self.ready.cancel()
        else:
            self.owner.active -= 1
        self.owner.dispatch()


class Admission:
    def __init__(self, active_limit, queue_limit, *, ready=True):
        self.active_limit, self.queue_limit = active_limit, queue_limit
        self.active = 0
        self.waiting = deque()
        self.ready = ready

    def reserve(self):
        # No awaits: check, reservation and dispatch are atomic in this loop.
        if (not self.ready or self.active >= self.active_limit or self.waiting) and len(self.waiting) >= self.queue_limit:
            raise QueueFull
        ticket = Ticket(self)
        self.waiting.append(ticket)
        self.dispatch()
        return ticket

    def dispatch(self):
        while self.ready and self.waiting and self.active < self.active_limit:
            ticket = self.waiting.popleft()
            self.active += 1
            ticket.granted = perf_counter()
            ticket.ready.set_result(None)

    def set_ready(self, ready):
        self.ready = ready
        self.dispatch()

    def snapshot(self):
        return {"active": self.active, "queued": len(self.waiting),
                "max_inflight": self.active_limit, "max_queued": self.queue_limit,
                "ready": self.ready}
