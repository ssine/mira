import asyncio
import json
from pathlib import Path
import tempfile
import unittest
import httpx
from gateway import Replica,ReplicaPool,PoolTransport,LeasedStream,ThreadContext,thread_key


class RoutingTest(unittest.IsolatedAsyncioTestCase):
    async def test_reload_drains_without_closing_active_stream_and_updates_address(self):
        pool=self.pool();tokens=list(range(128))
        active=await pool.acquire('thread',tokens,10)
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'config.json'
            rows=[{'name':r.name,'url':r.url,'capacity':r.capacity,'context_tokens':r.context,'enabled':r.name!=active.name} for r in pool.replicas]
            path.write_text(json.dumps({'replicas':rows}))
            transport=PoolTransport(pool,routing_config=str(path));transport.refresh_routes()
            self.assertEqual(1,active.active);self.assertTrue(active.draining)
            other=await pool.acquire('next',tokens,10);self.assertNotEqual(active.name,other.name)
            await pool.release(other,'next',tokens,True);await pool.release(active,'thread',tokens,True)
            for row in rows:
                if row['name']==active.name:row.update(enabled=True,url='http://replacement:8007')
            path.write_text(json.dumps({'replicas':rows}));transport.refresh_routes()
            self.assertFalse(active.draining);self.assertFalse(active.healthy)
            self.assertEqual('http://replacement:8007',active.url);self.assertNotIn('thread',pool.hints)
            path.write_text('broken');transport.refresh_routes()
            self.assertTrue(all(r.draining for r in pool.replicas));self.assertFalse(transport.configuration_valid)
            rows[0]['url']='http://invalid:port'
            path.write_text(json.dumps({'replicas':rows}));transport.refresh_routes()
            self.assertFalse(transport.configuration_valid)
            await transport.aclose()

    async def test_child_thread_header_takes_precedence_over_shared_session(self):
        keys=[]
        async def capture(scope, receive, send): keys.append(thread_key.get())
        context=ThreadContext(capture)
        for child in [b'child-a',b'child-b']:
            await context({'headers':[(b'thread-id',child),(b'session-id',b'shared')]},None,None)
        self.assertNotEqual(keys[0],keys[1]);self.assertEqual('',thread_key.get())

    def pool(self):
        return ReplicaPool([Replica('a','http://a',b'a'*48,1,1048576,True),Replica('b','http://b',b'b'*48,1,1048576,True)])

    async def test_distributes_and_retains_prefix_affinity(self):
        pool=self.pool();tokens=list(range(20000))
        a=await pool.acquire('parent',tokens,100)
        b=await pool.acquire('child',tokens,100)
        self.assertNotEqual(a.name,b.name)
        await pool.release(a,'parent',tokens,True)
        await pool.release(b,'child',tokens,True)
        next_a=await pool.acquire('parent',tokens+[7],100)
        self.assertEqual(a.name,next_a.name)
        await pool.release(next_a,'parent',tokens,True)

    async def test_full_warm_replica_uses_other_available_replica(self):
        pool=self.pool();tokens=list(range(20000))
        warm=await pool.acquire('thread',tokens,100)
        await pool.release(warm,'thread',tokens,True)
        occupied=await pool.acquire('thread',tokens,100)
        other=await pool.acquire('thread',tokens,100)
        self.assertNotEqual(occupied.name,other.name)
        await pool.release(occupied,'thread',tokens,False)
        await pool.release(other,'thread',tokens,False)

    async def test_cancelled_waiter_leaks_no_capacity(self):
        pool=self.pool();tokens=[1]
        a=await pool.acquire('',tokens,1);b=await pool.acquire('',tokens,1)
        pending=asyncio.create_task(pool.acquire('',tokens,1))
        await asyncio.sleep(0)
        self.assertEqual(1,pool.waiting)
        pending.cancel()
        with self.assertRaises(asyncio.CancelledError):await pending
        self.assertEqual(0,pool.waiting)
        await pool.release(a,'',tokens,False);await pool.release(b,'',tokens,False)
        self.assertEqual(0,sum(r.active for r in pool.replicas))

    async def test_excludes_incompatible_or_unhealthy_replica(self):
        pool=self.pool();pool.replicas[0].context=100
        r=await pool.acquire('',list(range(100)),1)
        self.assertEqual('b',r.name)
        await pool.release(r,'',[],False)
        pool.replicas[1].healthy=False
        with self.assertRaises(httpx.ConnectError):await pool.acquire('',list(range(100)),1)

    async def test_failed_send_is_never_replayed(self):
        pool=self.pool();calls=[]
        async def fail(request):
            calls.append(request)
            raise httpx.ReadError('synthetic stream failure')
        transport=PoolTransport(pool,transport=httpx.MockTransport(fail))
        async with httpx.AsyncClient(transport=transport,base_url='http://gateway') as client:
            with self.assertRaises(httpx.ReadError):await client.post('/recipe/v1/completions',json={'prompt':[1],'max_tokens':1})
        self.assertEqual(1,len(calls));self.assertEqual(0,sum(r.active for r in pool.replicas))

    async def test_stream_close_releases_lease_and_replaces_credentials(self):
        pool=self.pool();seen=[]
        async def respond(request):
            seen.append(request)
            return httpx.Response(200,content=b'data: {"choices":[{"finish_reason":"stop"}]}\n\ndata: [DONE]\n\n')
        transport=PoolTransport(pool,transport=httpx.MockTransport(respond))
        token=thread_key.set('thread')
        try:
            async with httpx.AsyncClient(transport=transport,base_url='http://gateway') as client:
                async with client.stream('POST','/recipe/v1/completions',json={'prompt':list(range(64)),'max_tokens':1},headers={'Authorization':'Bearer client-key'}) as response:
                    self.assertEqual(1,sum(r.active for r in pool.replicas))
                    self.assertIn(b'[DONE]',await response.aread())
                self.assertEqual(0,sum(r.active for r in pool.replicas))
        finally:thread_key.reset(token)
        self.assertEqual('Bearer '+'a'*48,seen[0].headers['authorization'])
        self.assertEqual('a',pool.hints['thread'][0])

    async def test_recipe_close_before_http_eof_keeps_hint(self):
        pool=self.pool();tokens=list(range(64))
        replica=await pool.acquire('thread',tokens,1)
        class Stream(httpx.AsyncByteStream):
            async def __aiter__(self):
                yield b'data: {"choices":[{"finish_reason":"stop"}]}\n\ndata: [DO'
                yield b'NE]\n\n'
                raise AssertionError('Recipe must close before reading HTTP EOF')
        stream=LeasedStream(Stream(),pool,replica,'thread',tokens)
        async for chunk in stream:
            if b'NE]' in chunk: break
        await stream.aclose();await stream.aclose()
        self.assertEqual(0,replica.active)
        self.assertEqual('a',pool.hints['thread'][0])

    async def test_truncated_or_error_stream_does_not_warm_hint(self):
        for body in (b'data: {"choices":[]}\n\n',b'data: {"error":"failed"}\n\ndata: [DONE]\n\n'):
            pool=self.pool();tokens=list(range(64))
            replica=await pool.acquire('thread',tokens,1)
            stream=LeasedStream(httpx.ByteStream(body),pool,replica,'thread',tokens)
            async for _ in stream: pass
            await stream.aclose()
            self.assertEqual(0,replica.active);self.assertFalse(pool.hints)

    async def test_image_expanded_context_is_used_for_eligibility(self):
        pool=self.pool();pool.replicas[0].context=200
        r=await pool.acquire('',[1],10,prompt_tokens=300)
        self.assertEqual('b',r.name)
        await pool.release(r,'',[],False)


if __name__=='__main__':unittest.main()
