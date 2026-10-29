"""Optional SDK compatibility smoke test. Uses a local fake upstream, no account."""

import argparse
import base64
import datetime
import json
import os
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

from config_helpers import write_account, write_config
from media_fixtures import png_fixture


class Upstream(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        assert self.path == "/manifest"
        assert "Authorization" not in self.headers
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"version: 2.30.1\n")

    def do_POST(self):
        payload = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        assert self.headers["Authorization"] == "Bearer fixture-access"
        if self.path == "/v1internal:fetchAvailableModels":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            if payload["project"] == "fixture-project-b":
                # The second account has a catalog of its own.
                self.wfile.write(
                    b'{"models":{"gemini-b":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"}},'
                    b'"agentModelSorts":[{"groups":[{"modelIds":["gemini-b"]}]}]}'
                )
                return
            self.wfile.write(
                b'{"models":{"gemini-test":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},'
                b'"gemini-canvas":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},'
                b'"gemini-unlisted":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},'
                b'"tab_preview":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},'
                b'"claude-test":{"apiProvider":"API_PROVIDER_ANTHROPIC_VERTEX"}},'
                b'"agentModelSorts":[{"groups":[{"modelIds":["claude-test","gemini-test"]}]}],'
                b'"imageGenerationModelIds":["gemini-canvas"]}'
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
        is_image = payload["requestType"] == "image_gen"
        if is_image:
            assert payload["model"] == "gemini-canvas"
            assert request["generationConfig"]["responseModalities"] == [
                "TEXT",
                "IMAGE",
            ]
            parts = [
                {
                    "inlineData": {
                        "mimeType": "image/png",
                        "data": base64.b64encode(png_fixture()).decode(),
                    }
                }
            ]
        elif request.get("tools") and not has_result:
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
        if is_image:
            body["response"]["usageMetadata"]["totalTokenCount"] = 15
            body["response"]["usageMetadata"]["promptTokensDetails"] = [
                {"modality": "TEXT", "tokenCount": 9}
            ]
        self.wfile.write(("data: " + json.dumps(body) + "\n\n").encode())


def check():
    upstream = ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    try:
        with tempfile.TemporaryDirectory(prefix="agy2api-sdk-") as directory:
            root = pathlib.Path(directory)
            binary = root / ("agy2api.exe" if os.name == "nt" else "agy2api")
            upstream_url = f"http://127.0.0.1:{upstream.server_port}"
            # Same CLI entry point and config loader as the release binary.
            # Only test builds substitute service addresses, with no runtime
            # environment or user-config escape hatch in the shipped program.
            subprocess.run(
                [
                    "go",
                    "build",
                    "-trimpath",
                    "-ldflags",
                    f"-X main.upstreamURL={upstream_url} "
                    f"-X github.com/zclkkk/agy2api/internal/antigravity.manifestURL={upstream_url}/manifest",
                    "-o",
                    str(binary),
                    "./cmd/agy2api",
                ],
                cwd=pathlib.Path(__file__).resolve().parents[1],
                check=True,
            )
            with socket.socket() as sock:
                sock.bind(("127.0.0.1", 0))
                port = sock.getsockname()[1]
            data = root / "data"
            data.mkdir(mode=0o700)
            expiry = (
                datetime.datetime.now(datetime.timezone.utc)
                + datetime.timedelta(hours=1)
            ).isoformat()
            accounts = {}
            for user, key, project in (
                ("101", "fixture-key", "fixture-project"),
                ("102", "fixture-key-b", "fixture-project-b"),
            ):
                token = {
                    "access_token": "fixture-access",
                    "refresh_token": "fixture-refresh",
                    "token_type": "Bearer",
                    "expiry": expiry,
                }
                credentials = {"token": token, "project_id": project}
                directory = write_account(
                    data, user, key, credentials, f"user{user}@example.com"
                )
                accounts[key] = directory.name
            config = root / "config.toml"
            write_config(config, {"listen": f"127.0.0.1:{port}", "data_dir": str(data)})
            env = dict(os.environ)
            for name in ("AGY2API_LISTEN", "AGY2API_DATA_DIR", "AGY2API_PROXY_URL"):
                env.pop(name, None)
            with (root / "service.log").open("w+") as log:
                process = subprocess.Popen(
                    [str(binary), "serve", "--config", str(config)],
                    env=env,
                    stderr=log,
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
                        listed = [m.id for m in client.models.list().data]
                        assert listed == [
                            "gemini-test",
                            "gemini-canvas",
                        ]
                        # Each key reaches its own account.
                        with openai.OpenAI(
                            api_key="fixture-key-b",
                            base_url=base + "/v1",
                            max_retries=0,
                        ) as other:
                            assert [m.id for m in other.models.list().data] == [
                                "gemini-b"
                            ]
                        with openai.OpenAI(
                            api_key="unknown-key", base_url=base + "/v1", max_retries=0
                        ) as unknown:
                            try:
                                unknown.models.list()
                            except openai.AuthenticationError:
                                pass
                            else:
                                raise AssertionError("unknown key accepted")

                        def cli(command):
                            return subprocess.run(
                                [str(binary), command, "--config", str(config)],
                                env=env,
                                capture_output=True,
                                text=True,
                                timeout=15,
                                check=True,
                            ).stdout

                        # Both accounts share a creation time, so they list by ID.
                        keys = sorted(accounts, key=accounts.get)
                        header, *rows = cli("accounts").splitlines()
                        assert header.split()[:2] == ["ID", "EMAIL"]
                        assert [(r.split()[0], r.split()[3]) for r in rows] == [
                            (accounts[key], key) for key in keys
                        ]
                        sections = cli("quota").strip().split("\n\n")
                        catalogs = {
                            "fixture-key": listed,
                            "fixture-key-b": ["gemini-b"],
                        }
                        for section, key in zip(sections, keys, strict=True):
                            models = catalogs[key]
                            title, header, *rows = section.splitlines()
                            assert title.split()[0] == accounts[key]
                            assert header.split() == ["MODEL", "QUOTA", "RESETS"]
                            assert [row.split()[0] for row in rows] == models
                        assert (
                            client.models.retrieve("gemini-unlisted").id
                            == "gemini-unlisted"
                        )
                        unlisted = client.responses.create(
                            model="gemini-unlisted", input="hello", store=False
                        )
                        assert unlisted.output_text == "Hello from agy2api"
                        generated = client.images.generate(
                            model="gemini-canvas",
                            prompt="draw",
                            n=2,
                        )
                        type(generated).model_validate(generated.model_dump())
                        assert (
                            len(generated.data) == 2
                            and generated.usage.total_tokens == 30
                        )
                        assert (
                            base64.b64decode(generated.data[0].b64_json)
                            == png_fixture()
                        )
                        for images in [
                            ("input.png", png_fixture(), "image/png"),
                            [
                                ("one.png", png_fixture(), "image/png"),
                                ("two.png", png_fixture(), "image/png"),
                            ],
                        ]:
                            edited = client.images.edit(
                                model="gemini-canvas",
                                prompt="edit",
                                image=images,
                                output_format="jpeg",
                                output_compression=85,
                            )
                            type(edited).model_validate(edited.model_dump())
                            assert edited.output_format == "jpeg"
                            assert base64.b64decode(edited.data[0].b64_json).startswith(
                                b"\xff\xd8"
                            )
                        edited = client.post(
                            "/images/edits",
                            cast_to=openai.types.ImagesResponse,
                            body={
                                "model": "gemini-canvas",
                                "prompt": "edit",
                                "images": [
                                    {
                                        "image_url": "data:image/png;base64,"
                                        + base64.b64encode(png_fixture()).decode()
                                    }
                                ],
                            },
                        )
                        type(edited).model_validate(edited.model_dump())
                        assert (
                            base64.b64decode(edited.data[0].b64_json) == png_fixture()
                        )
                        try:
                            client.images.generate(model="gemini-test", prompt="draw")
                        except openai.BadRequestError as error:
                            assert error.param == "model"
                        else:
                            raise AssertionError(
                                "conversation model accepted for image generation"
                            )
                        result = client.responses.create(
                            model="gemini-test", input="hello"
                        )
                        type(result).model_validate(result.model_dump())
                        assert result.output_text == "Hello from agy2api"
                        assert client.responses.retrieve(result.id).id == result.id
                        input_page = client.responses.input_items.list(result.id)
                        type(input_page).model_validate(input_page.model_dump())
                        input_items = input_page.data
                        assert len(input_items) == 1
                        type(input_items[0]).model_validate(input_items[0].model_dump())
                        assert input_items[0].id
                        assert input_items[0].content[0].type == "input_text"
                        assert input_items[0].content[0].text == "hello"
                        followup = client.responses.create(
                            model="gemini-test",
                            previous_response_id=result.id,
                            input="again",
                        )
                        assert followup.output_text == "Hello from agy2api"
                        history = list(
                            client.responses.input_items.list(
                                followup.id, limit=1, order="asc"
                            )
                        )
                        assert len(history) == 3
                        assert history[0].id == input_items[0].id
                        assert [item.content[0].text for item in history] == [
                            "hello",
                            "Hello from agy2api",
                            "again",
                        ]
                        compacted = client.responses.compact(
                            model="gemini-test",
                            input=[
                                {
                                    "role": "user",
                                    "content": "Remember this conversation.",
                                }
                            ],
                        )
                        type(compacted).model_validate(compacted.model_dump())
                        capsule = compacted.output[0].encrypted_content
                        assert capsule.startswith("agy2.")
                        resumed = client.responses.create(
                            model="gemini-test",
                            input=[
                                *compacted.output,
                                {
                                    "role": "user",
                                    "content": "Continue after compacting.",
                                },
                            ],
                            store=False,
                        )
                        assert resumed.output_text == "Hello from agy2api"
                        tampered = (
                            capsule[:5]
                            + ("A" if capsule[5] != "A" else "B")
                            + capsule[6:]
                        )
                        try:
                            client.responses.create(
                                model="gemini-test",
                                input=[
                                    {
                                        "type": "compaction",
                                        "encrypted_content": tampered,
                                    },
                                    {"role": "user", "content": "continue"},
                                ],
                                store=False,
                            )
                        except openai.BadRequestError as error:
                            assert error.param == "input"
                        else:
                            raise AssertionError("modified compaction was accepted")
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
                        for tool_type in ("web_search", "image_generation"):
                            try:
                                client.responses.create(
                                    model="gemini-test",
                                    input="hello",
                                    tools=[{"type": tool_type}],
                                )
                            except openai.BadRequestError as error:
                                assert "unsupported" in str(error)
                            else:
                                raise AssertionError(
                                    f"hosted tool accepted: {tool_type}"
                                )
                    with anthropic.Anthropic(
                        api_key="fixture-key", base_url=base, max_retries=0
                    ) as client:
                        # Image generation models only serve the OpenAI Images API.
                        assert [m.id for m in client.models.list().data] == [
                            "gemini-test"
                        ]
                        messages = [{"role": "user", "content": "hello"}]
                        result = client.messages.create(
                            model="gemini-test", max_tokens=1024, messages=messages
                        )
                        type(result).model_validate(result.model_dump())
                        assert result.content[0].text == "Hello from agy2api"
                        try:
                            client.messages.create(
                                model="gemini-test",
                                max_tokens=1024,
                                messages=messages,
                                tools=[
                                    {
                                        "type": "web_search_20250305",
                                        "name": "web_search",
                                    }
                                ],
                            )
                        except anthropic.BadRequestError as error:
                            assert "unsupported" in str(error)
                        else:
                            raise AssertionError("hosted search accepted")
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
    argparse.ArgumentParser(description=__doc__).parse_args()
    check()
