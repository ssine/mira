"""Authenticated internal relay to a loopback-only vLLM Recipe plugin."""
import argparse
from contextlib import asynccontextmanager
from anyio import CancelScope
import httpx
from fastapi import FastAPI,Request
from fastapi.responses import Response
from service import AuthAndCapacity,ClosingStreamingResponse,read_key
from admission import Admission


def relay_app(key_file, *, transport=None, max_inflight=8):
    @asynccontextmanager
    async def lifespan(app):
        async with httpx.AsyncClient(base_url='http://127.0.0.1:8000',trust_env=False,transport=transport,
            timeout=httpx.Timeout(connect=5,read=600,write=30,pool=5),
            limits=httpx.Limits(max_connections=10,max_keepalive_connections=10,keepalive_expiry=1800)) as client:
            app.state.client=client
            yield
    app=FastAPI(lifespan=lifespan,docs_url=None,redoc_url=None,openapi_url=None)
    app.state.admission=Admission(max_inflight,0)
    app.add_middleware(AuthAndCapacity,key=read_key(key_file),admission=app.state.admission,
        inference_paths=('/recipe/v1/completions','/v1/completions'))
    @app.api_route('/{path:path}',methods=['GET','POST'])
    async def relay(path,request:Request):
        allowed=(request.method=='GET' and path=='health') or (request.method=='POST' and path in ('recipe/v1/completions','v1/completions'))
        if not allowed:return Response(status_code=404)
        body=bytearray()
        async for chunk in request.stream():
            if len(body)+len(chunk)>64*1024*1024:return Response(status_code=413)
            body.extend(chunk)
        try:
            upstream=await request.app.state.client.send(request.app.state.client.build_request(request.method,'/'+path,content=bytes(body),headers={'Content-Type':'application/json'}),stream=True)
        except httpx.HTTPError:return Response(status_code=502)
        async def content():
            try:
                async for chunk in upstream.aiter_raw():yield chunk
            finally:
                with CancelScope(shield=True):await upstream.aclose()
        return ClosingStreamingResponse(content(),status_code=upstream.status_code,
            headers={k:v for k,v in upstream.headers.items() if k.lower() in ('content-type','cache-control','x-accel-buffering')})
    return app


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--key-file',required=True);p.add_argument('--host',required=True);p.add_argument('--port',type=int,default=8007)
    a=p.parse_args()
    import uvicorn
    uvicorn.run(relay_app(a.key_file),host=a.host,port=a.port,access_log=False,server_header=False,timeout_keep_alive=1800)
