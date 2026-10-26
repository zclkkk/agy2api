"""Optional SDK compatibility smoke test. Uses a local fake upstream, no account."""

import argparse
import datetime
import json
import pathlib
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import anthropic
import openai


class Upstream(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        payload = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        assert self.headers["Authorization"] == "Bearer fixture-access"
        if self.path == "/v1internal:fetchAvailableModels":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(
                b'{"models":{"gemini-test":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},'
                b'"claude-test":{"apiProvider":"API_PROVIDER_ANTHROPIC_VERTEX"}}}'
            )
            return
        if self.path == "/v1internal:countTokens":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"totalTokens":17}')
            return
        assert self.path == "/v1internal:streamGenerateContent?alt=sse"
        request = payload["request"]
        has_result = any(
            "functionResponse" in p for c in request["contents"] for p in c["parts"]
        )
        if request.get("tools") and not has_result:
            parts = [
                {
                    "text": "Plan",
                    "thought": True,
                    "thoughtSignature": "fixture-thought",
                },
                {
                    "functionCall": {
                        "id": "native-call",
                        "name": "lookup",
                        "args": {"city": "Shanghai"},
                    },
                    "thoughtSignature": "fixture-tool",
                },
            ]
        else:
            parts = [{"text": "Hello from agy2api"}]
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        body = {
            "response": {
                "candidates": [
                    {
                        "content": {"role": "model", "parts": parts},
                        "finishReason": "STOP",
                    }
                ],
                "usageMetadata": {
                    "promptTokenCount": 9,
                    "candidatesTokenCount": 4,
                    "thoughtsTokenCount": 2,
                },
            }
        }
        self.wfile.write(("data: " + json.dumps(body) + "\n\n").encode())


def check(binary):
    upstream = ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    try:
        with tempfile.TemporaryDirectory(prefix="agy2api-sdk-") as directory:
            root = pathlib.Path(directory)
            with socket.socket() as sock:
                sock.bind(("127.0.0.1", 0))
                port = sock.getsockname()[1]
            data = root / "data"
            data.mkdir()
            expiry = (
                datetime.datetime.now(datetime.timezone.utc)
                + datetime.timedelta(hours=1)
            ).isoformat()
            (data / "credentials.json").write_text(
                json.dumps(
                    {
                        "token": {
                            "access_token": "fixture-access",
                            "refresh_token": "fixture-refresh",
                            "token_type": "Bearer",
                            "expiry": expiry,
                        },
                        "project_id": "fixture-project",
                    }
                )
            )
            config = root / "config.json"
            config.write_text(
                json.dumps(
                    {
                        "listen": f"127.0.0.1:{port}",
                        "data_dir": str(data),
                        "api_key": "fixture-key",
                        "upstream_url": f"http://127.0.0.1:{upstream.server_port}",
                    }
                )
            )
            with (root / "service.log").open("w+") as log:
                process = subprocess.Popen(
                    [str(binary), "serve", "--config", str(config)], stderr=log
                )
                try:
                    base = f"http://127.0.0.1:{port}"
                    deadline = time.monotonic() + 10
                    while True:
                        if process.poll() is not None:
                            log.seek(0)
                            raise RuntimeError(log.read())
                        try:
                            with urllib.request.urlopen(base + "/healthz", timeout=1):
                                break
                        except urllib.error.URLError:
                            if time.monotonic() > deadline:
                                raise
                            time.sleep(0.02)
                    with openai.OpenAI(
                        api_key="fixture-key", base_url=base + "/v1", max_retries=0
                    ) as client:
                        assert [m.id for m in client.models.list().data] == [
                            "gemini-test"
                        ]
                        result = client.responses.create(
                            model="gemini-test", input="hello"
                        )
                        type(result).model_validate(result.model_dump())
                        assert result.output_text == "Hello from agy2api"
                        assert client.responses.retrieve(result.id).id == result.id
                        assert (
                            len(client.responses.input_items.list(result.id).data) == 1
                        )
                        followup = client.responses.create(
                            model="gemini-test",
                            previous_response_id=result.id,
                            input="again",
                        )
                        assert followup.output_text == "Hello from agy2api"
                        with client.responses.stream(
                            model="gemini-test", input="hello", store=False
                        ) as stream:
                            for _ in stream:
                                pass
                            assert (
                                stream.get_final_response().output_text
                                == "Hello from agy2api"
                            )
                        tool = {
                            "type": "function",
                            "name": "lookup",
                            "parameters": {
                                "type": "object",
                                "properties": {"city": {"type": "string"}},
                            },
                            "strict": False,
                        }
                        with client.responses.stream(
                            model="gemini-test", input="lookup", tools=[tool]
                        ) as stream:
                            for _ in stream:
                                pass
                            result = stream.get_final_response()
                        type(result).model_validate(result.model_dump())
                        call = next(
                            item
                            for item in result.output
                            if item.type == "function_call"
                        )
                        result2 = client.responses.create(
                            model="gemini-test",
                            previous_response_id=result.id,
                            input=[
                                {
                                    "type": "function_call_output",
                                    "call_id": call.call_id,
                                    "output": "found",
                                }
                            ],
                        )
                        assert result2.output_text == "Hello from agy2api"
                        client.responses.delete(result.id)
                        try:
                            client.responses.retrieve(result.id)
                        except openai.NotFoundError:
                            pass
                        else:
                            raise AssertionError("deleted response is still accessible")
                    with anthropic.Anthropic(
                        api_key="fixture-key", base_url=base, max_retries=0
                    ) as client:
                        messages = [{"role": "user", "content": "hello"}]
                        result = client.messages.create(
                            model="gemini-test", max_tokens=1024, messages=messages
                        )
                        type(result).model_validate(result.model_dump())
                        assert result.content[0].text == "Hello from agy2api"
                        assert (
                            client.messages.count_tokens(
                                model="gemini-test", messages=messages
                            ).input_tokens
                            == 17
                        )
                        with client.messages.stream(
                            model="gemini-test", max_tokens=1024, messages=messages
                        ) as stream:
                            assert "".join(stream.text_stream) == "Hello from agy2api"
                            type(result).model_validate(
                                stream.get_final_message().model_dump()
                            )
                        tools = [
                            {
                                "name": "lookup",
                                "input_schema": {
                                    "type": "object",
                                    "properties": {"city": {"type": "string"}},
                                },
                            }
                        ]
                        with client.messages.stream(
                            model="gemini-test",
                            max_tokens=1024,
                            messages=messages,
                            tools=tools,
                        ) as stream:
                            for _ in stream:
                                pass
                            result = stream.get_final_message()
                        assert result.stop_reason == "tool_use"
                        call = next(
                            block
                            for block in result.content
                            if block.type == "tool_use"
                        )
                        messages += [
                            {
                                "role": "assistant",
                                "content": [
                                    b.model_dump(exclude_none=True)
                                    for b in result.content
                                ],
                            },
                            {
                                "role": "user",
                                "content": [
                                    {
                                        "type": "tool_result",
                                        "tool_use_id": call.id,
                                        "content": "found",
                                    }
                                ],
                            },
                        ]
                        result = client.messages.create(
                            model="gemini-test",
                            max_tokens=1024,
                            messages=messages,
                            tools=tools,
                        )
                        assert result.content[0].text == "Hello from agy2api"
                    print(
                        f"SDK checks passed: openai={openai.__version__}, anthropic={anthropic.__version__}"
                    )
                finally:
                    process.terminate()
                    try:
                        process.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait()
    finally:
        upstream.shutdown()
        upstream.server_close()


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--binary", type=pathlib.Path, default=pathlib.Path("bin/agy2api")
    )
    check(parser.parse_args().binary.resolve())
