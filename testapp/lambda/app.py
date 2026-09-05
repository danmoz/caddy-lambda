import asyncio
import contextvars

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, Response
from mangum import Mangum

app = FastAPI()
current_event = contextvars.ContextVar("current_event", default=None)


@app.get("/health")
async def health():
    return {"ok": True}


@app.api_route("/echo", methods=["GET", "POST"])
async def echo(request: Request):
    query = {key: request.query_params.getlist(key) for key in request.query_params}
    body = await request.body()
    event = current_event.get() or {}
    return {
        "method": request.method,
        "query": query,
        "headers": {
            "authorization": request.headers.get("authorization"),
            "x-test": request.headers.get("x-test"),
        },
        "body": body.decode("utf-8", errors="replace"),
        "event": {
            "body": event.get("body"),
            "isBase64Encoded": event.get("isBase64Encoded", False),
            "headers": event.get("headers", {}),
        },
    }


@app.get("/status/{status_code}")
async def status(status_code: int):
    return JSONResponse({"status": status_code}, status_code=status_code)


@app.get("/cookies")
async def cookies():
    response = JSONResponse({"ok": True})
    response.set_cookie("session", "abc")
    response.set_cookie("theme", "dark")
    return response


@app.get("/binary")
async def binary():
    return Response(b"\x00\x01\xfe\xff", media_type="application/octet-stream")


@app.get("/timeout")
async def timeout():
    await asyncio.sleep(60)
    return {"ok": True}


@app.get("/lambda-error")
async def lambda_error():
    raise RuntimeError("fixture failure")


_mangum_handler = Mangum(app)


def handler(event, context):
    token = current_event.set(event)
    try:
        return _mangum_handler(event, context)
    finally:
        current_event.reset(token)
