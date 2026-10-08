"""Recipe gateway with bounded replica admission and per-thread cache hints."""
import argparse
from array import array
import asyncio
from collections import OrderedDict
from contextlib import asynccontextmanager
from contextvars import ContextVar
from dataclasses import dataclass
import hashlib
import hmac
import json
from pathlib import Path
from time import monotonic

from anyio import CancelScope
import httpx

from service import Settings, create_app, read_key

thread_key = ContextVar('deepseek_gateway_thread', default='')


class ThreadContext:
    def __init__(self, app): self.app = app
    async def __call__(self, scope, receive, send):
        headers = dict(scope.get('headers', []))
        # Children have their own thread-id even when session-id is shared.
        value = headers.get(b'thread-id', headers.get(b'session-id', b''))
        token = thread_key.set(hashlib.sha256(value).hexdigest() if 0 < len(value) <= 256 else '')
        try: await self.app(scope, receive, send)
        finally: thread_key.reset(token)


class KeyAliases:
    def __init__(self, app, primary, aliases):
        self.app, self.primary, self.aliases = app, primary, aliases
    async def __call__(self, scope, receive, send):
        if scope['type'] == 'http':
            auth = [v for k,v in scope['headers'] if k == b'authorization']
            parts = auth[0].split() if len(auth) == 1 else []
            if len(parts) == 2 and parts[0].lower() == b'bearer' and any(hmac.compare_digest(parts[1], key) for key in self.aliases):
                scope = {**scope, 'headers': [(k,v) for k,v in scope['headers'] if k != b'authorization'] + [(b'authorization', b'Bearer '+self.primary)]}
        await self.app(scope, receive, send)


@dataclass
class Replica:
    name: str
    url: str
    key: bytes
    capacity: int
    context: int
    healthy: bool = False
    active: int = 0
    requests: int = 0
    failures: int = 0
    draining: bool = False


def replica_url(value):
    url = httpx.URL(value)
    if url.scheme not in ('http', 'https') or not url.host or url.username or url.password or url.query or url.fragment:
        raise ValueError('Invalid replica URL')
    return str(url).rstrip('/')


def prefix_digest(tokens, length):
    return hashlib.sha256(array('I', tokens[:length]).tobytes()).digest()


class ReplicaPool:
    def __init__(self, replicas, *, hint_limit=8192, hint_ttl=1800):
        if not replicas or len({r.name for r in replicas}) != len(replicas) or any(r.capacity < 1 or r.context < 1 for r in replicas):
            raise ValueError('At least one valid replica is required')
        self.replicas = replicas
        self.condition = asyncio.Condition()
        self.hints = OrderedDict()
        self.hint_limit, self.hint_ttl = hint_limit, hint_ttl
        self.waiting = 0

    async def acquire(self, key, tokens, output, prompt_tokens=None):
        input_count = max(len(tokens), prompt_tokens or 0)
        hint = self.hints.get(key) if key else None
        cached = 0
        if hint and monotonic()-hint[3] < self.hint_ttl and hint[1] <= len(tokens):
            if prefix_digest(tokens, hint[1]) == hint[2]: cached = hint[1]
        async with self.condition:
            self.waiting += 1
            try:
                while True:
                    eligible = [r for r in self.replicas if r.healthy and not r.draining and input_count+output <= r.context]
                    if not eligible:
                        raise httpx.ConnectError('No healthy compatible inference replica')
                    available = [r for r in eligible if r.active < r.capacity]
                    if available:
                        # Cached location is only a hint. Existing decode work has
                        # a cost, and a full replica never blocks an available one.
                        def cost(r):
                            uncached = input_count - (cached if hint and r.name == hint[0] else 0)
                            return (uncached + r.active*8192, r.active, r.requests)
                        replica = min(available, key=cost)
                        replica.active += 1; replica.requests += 1
                        return replica
                    await self.condition.wait()
            finally: self.waiting -= 1

    async def release(self, replica, key, tokens, success):
        async with self.condition:
            replica.active -= 1
            if success and key:
                length = len(tokens)//64*64
                self.hints[key] = (replica.name, length, prefix_digest(tokens, length), monotonic())
                self.hints.move_to_end(key)
                while len(self.hints) > self.hint_limit: self.hints.popitem(last=False)
            self.condition.notify_all()

    def snapshot(self):
        return {'waiting':self.waiting, 'cache_hints':len(self.hints), 'replicas':[
            {'name':r.name,'healthy':r.healthy,'draining':r.draining,'active':r.active,'capacity':r.capacity,
             'context_tokens':r.context,'requests':r.requests,'failures':r.failures}
            for r in self.replicas]}


class LeasedStream(httpx.AsyncByteStream):
    def __init__(self, original, pool, replica, key, tokens):
        self.original, self.pool, self.replica = original, pool, replica
        self.key, self.tokens = key, tokens
        self.closed = False; self.complete = False
        self.line = bytearray()
        self.finished = False
        self.failed = False

    def observe(self, data):
        # Recipe closes on [DONE], before HTTP EOF. Observe bounded SSE lines
        # so a completed response can still teach the next request its location.
        self.line.extend(data)
        while b'\n' in self.line:
            line, _, rest = self.line.partition(b'\n')
            self.line = bytearray(rest)
            if not line.startswith(b'data: '): continue
            value = line[6:].strip()
            if value == b'[DONE]':
                self.complete = self.finished and not self.failed
                continue
            try:
                event = json.loads(value)
                self.failed |= bool(event.get('error'))
                self.finished |= any(c.get('finish_reason') in ('stop', 'length') for c in event.get('choices', []))
            except (ValueError, AttributeError, TypeError): self.failed = True
        if len(self.line) > 1024*1024:
            self.line.clear()
            self.failed = True

    async def __aiter__(self):
        async for data in self.original:
            self.observe(data)
            yield data
    async def aclose(self):
        if self.closed: return
        self.closed = True
        with CancelScope(shield=True):
            try: await self.original.aclose()
            finally: await self.pool.release(self.replica, self.key, self.tokens, self.complete)


class PoolTransport(httpx.AsyncBaseTransport):
    def __init__(self, pool, *, transport=None, routing_config=None):
        self.pool = pool
        self.routing_config = routing_config
        self.configuration_valid = True
        self.transport = transport or httpx.AsyncHTTPTransport(retries=0,
            limits=httpx.Limits(max_connections=sum(r.capacity for r in pool.replicas), max_keepalive_connections=sum(r.capacity for r in pool.replicas),keepalive_expiry=1800))
        self.monitor = None
        self.closed = False

    async def start(self):
        self.health = httpx.AsyncClient(trust_env=False,timeout=3)
        await self.probe()
        self.monitor = asyncio.create_task(self.watch())

    def refresh_routes(self):
        if not self.routing_config: return
        try:
            with open(self.routing_config, 'rb') as handle: data = handle.read(1024*1024+1)
            if len(data) > 1024*1024: raise ValueError('Oversized configuration')
            rows = json.loads(data)['replicas']
            if len(rows) != len(self.pool.replicas): raise ValueError('Replica set changed')
            by_name = {r['name']:r for r in rows}
            if len(by_name) != len(rows): raise ValueError('Duplicate replica')
            updates = []
            for r in self.pool.replicas:
                row = by_name[r.name]
                if row.get('capacity', 8) != r.capacity or row['context_tokens'] != r.context:
                    raise ValueError('Replica capacity/context changes require a restart')
                enabled = row.get('enabled', True)
                if not isinstance(enabled, bool): raise ValueError('Invalid replica enabled flag')
                updates.append((r, replica_url(row['url']), not enabled))
            for r, url, draining in updates:
                if url != r.url or (r.draining and not draining):
                    r.healthy = False
                    for key in [k for k,v in self.pool.hints.items() if v[0] == r.name]: del self.pool.hints[key]
                r.url, r.draining = url, draining
            self.configuration_valid = True
        except (OSError, ValueError, KeyError, TypeError, httpx.InvalidURL):
            self.configuration_valid = False
            for r in self.pool.replicas: r.draining = True

    async def probe(self):
        self.refresh_routes()
        async def one(r):
            try:
                result = await self.health.get(r.url+'/health',headers={'Authorization':'Bearer '+r.key.decode()})
                healthy = result.status_code == 200
            except httpx.HTTPError: healthy = False
            async with self.pool.condition:
                r.healthy = healthy; self.pool.condition.notify_all()
        await asyncio.gather(*(one(r) for r in self.pool.replicas))

    async def watch(self):
        while True:
            await asyncio.sleep(2)
            await self.probe()

    async def handle_async_request(self, request):
        if request.url.path == '/health':
            healthy = any(r.healthy and not r.draining for r in self.pool.replicas)
            return httpx.Response(200 if healthy else 503, json=self.pool.snapshot())
        if request.url.path not in ('/recipe/v1/completions','/v1/completions'):
            return httpx.Response(404)
        payload = json.loads(request.content)
        tokens = payload['prompt']
        # Text token IDs alone cannot identify image cache contents.
        key = thread_key.get() if not payload.get('images') else ''
        replica = await self.pool.acquire(key,tokens,payload['max_tokens'],payload.get('expected_prompt_tokens'))
        headers = [(k,v) for k,v in request.headers.raw if k.lower() not in (b'authorization',b'host')]
        headers.append((b'authorization', b'Bearer '+replica.key))
        target = httpx.Request(request.method,replica.url+request.url.raw_path.decode(),headers=headers,
            content=request.content,extensions=request.extensions)
        try:
            response = await self.transport.handle_async_request(target)
            if response.status_code != 200:
                await response.aclose()
                replica.failures += 1
                await self.pool.release(replica,key,tokens,False)
                return httpx.Response(response.status_code)
            return httpx.Response(response.status_code,headers=response.headers,
                stream=LeasedStream(response.stream,self.pool,replica,key,tokens),extensions=response.extensions)
        except BaseException:
            replica.failures += 1
            replica.healthy = False
            with CancelScope(shield=True): await self.pool.release(replica,key,tokens,False)
            # Never retry after selecting/sending a model request.
            raise

    async def aclose(self):
        if self.closed: return
        self.closed = True
        if self.monitor:
            self.monitor.cancel(); await asyncio.gather(self.monitor,return_exceptions=True)
        if hasattr(self,'health'): await self.health.aclose()
        await self.transport.aclose()


def gateway_app(settings, replicas, aliases=(), *, routing_config=None):
    pool = ReplicaPool(replicas)
    transport = PoolTransport(pool,routing_config=routing_config)
    app = create_app(settings,transport=transport)
    original = app.router.lifespan_context
    @asynccontextmanager
    async def lifespan(app):
        await transport.start()
        try:
            async with original(app): yield
        finally: await transport.aclose()
    app.router.lifespan_context = lifespan
    app.add_middleware(ThreadContext)
    app.add_middleware(KeyAliases,primary=read_key(settings.key_file),aliases=aliases)
    app.state.replica_pool = pool
    @app.get('/health/routing')
    async def routing(): return {**pool.snapshot(), 'configuration_valid':transport.configuration_valid}
    return app


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config',required=True)
    args=parser.parse_args()
    config=json.loads(Path(args.config).read_text())
    replicas=[]
    for r in config['replicas']:
        replicas.append(Replica(r['name'],replica_url(r['url']),read_key(r['key_file']),r.get('capacity',8),r['context_tokens']))
    settings=Settings(tokenizer=config['tokenizer'],key_file=config['key_file'],context_tokens=config['context_tokens'],
        recipe_backend=True,max_inflight=sum(r.capacity for r in replicas),max_queued=config.get('max_queued',64),queue_timeout=1800)
    app=gateway_app(settings,replicas,[read_key(p) for p in config.get('alias_key_files',[])],routing_config=args.config)
    import uvicorn
    uvicorn.run(app,host='127.0.0.1',port=config.get('port',8006),access_log=False,server_header=False,timeout_keep_alive=1800)


if __name__=='__main__': main()
