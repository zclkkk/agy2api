"""Checks that the official SDKs accept every response of the real binary.

The CLI entry point runs against a fake upstream; no account is used. The Go
tests cover behavior. This covers what they cannot: the SDKs' response models
and error handling, and the binary's wiring of accounts and commands.
"""

import argparse
import base64
import contextlib
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


REPLY = "Hello from agy2api"


def validated(value):
    """Round-trip an SDK object through its model to catch schema drift."""
    type(value).model_validate(value.model_dump())
    return value


def expect(error_type, call, check=lambda error: True):
    try:
        call()
    except error_type as error:
        assert check(error), error
        return
    raise AssertionError(f"expected {error_type.__name__}")


def build(root, upstream_url):
    # Same CLI entry point and config loader as the release binary. Only test
    # builds substitute service addresses; the shipped program has no runtime
    # escape hatch for them.
    binary = root / ("agy2api.exe" if os.name == "nt" else "agy2api")
    flags = (
        f"-X main.upstreamURL={upstream_url} "
        "-X github.com/zclkkk/agy2api/internal/antigravity.manifestURL="
        f"{upstream_url}/manifest"
    )
    subprocess.run(
        [
            "go",
            "build",
            "-trimpath",
            "-ldflags",
            flags,
            "-o",
            str(binary),
            "./cmd/agy2api",
        ],
        cwd=pathlib.Path(__file__).resolve().parents[1],
        check=True,
    )
    return binary


def write_accounts(root, port):
    """Two accounts on different projects; the fake upstream gives each its
    own catalog. Returns the config path and the account ID of each key."""
    data = root / "data"
    data.mkdir(mode=0o700)
    expiry = (
        datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(hours=1)
    ).isoformat()
    ids = {}
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
        account = write_account(data, user, key, credentials, f"u{user}@example.com")
        ids[key] = account.name
    config = root / "config.toml"
    write_config(config, {"listen": f"127.0.0.1:{port}", "data_dir": str(data)})
    return config, ids


@contextlib.contextmanager
def serving(binary, config, env, base, log):
    process = subprocess.Popen(
        [str(binary), "serve", "--config", str(config)], env=env, stderr=log
    )
    try:
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
        yield
    finally:
        process.terminate()
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def check_accounts(base, cli, ids):
    """Each key reaches its own account; the CLI lists keys and quota."""
    catalogs = {
        "fixture-key": ["gemini-test", "gemini-canvas"],
        "fixture-key-b": ["gemini-b"],
    }
    for key, models in catalogs.items():
        with openai.OpenAI(api_key=key, base_url=base + "/v1", max_retries=0) as client:
            assert [m.id for m in validated(client.models.list()).data] == models
    with openai.OpenAI(
        api_key="unknown-key", base_url=base + "/v1", max_retries=0
    ) as client:
        expect(openai.AuthenticationError, client.models.list)
    # Both accounts share a creation time, so they list by ID.
    keys = sorted(ids, key=ids.get)
    header, *rows = cli("accounts").splitlines()
    assert header.split()[:2] == ["ID", "EMAIL"]
    assert [(row.split()[0], row.split()[3]) for row in rows] == [
        (ids[k], k) for k in keys
    ]
    for section, key in zip(cli("quota").strip().split("\n\n"), keys, strict=True):
        title, header, *rows = section.splitlines()
        assert title.split()[0] == ids[key]
        assert header.split() == ["MODEL", "QUOTA", "RESETS"]
        assert [row.split()[0] for row in rows] == catalogs[key]


def check_responses(client):
    assert validated(client.models.retrieve("gemini-unlisted")).id == "gemini-unlisted"
    result = validated(client.responses.create(model="gemini-test", input="hello"))
    assert result.output_text == REPLY
    assert validated(client.responses.retrieve(result.id)).id == result.id
    items = validated(client.responses.input_items.list(result.id)).data
    assert [(item.content[0].type, item.content[0].text) for item in items] == [
        ("input_text", "hello")
    ]
    followup = client.responses.create(
        model="gemini-test", previous_response_id=result.id, input="again"
    )
    history = list(client.responses.input_items.list(followup.id, limit=1, order="asc"))
    assert history[0].id == items[0].id
    assert [item.content[0].text for item in history] == ["hello", REPLY, "again"]
    compacted = validated(
        client.responses.compact(
            model="gemini-test",
            input=[{"role": "user", "content": "Remember this conversation."}],
        )
    )
    assert compacted.output[0].encrypted_content.startswith("agy2.")
    resumed = client.responses.create(
        model="gemini-test",
        input=[*compacted.output, {"role": "user", "content": "Continue."}],
        store=False,
    )
    assert resumed.output_text == REPLY
    with client.responses.stream(
        model="gemini-test", input="hello", store=False
    ) as stream:
        for _ in stream:
            pass
        assert validated(stream.get_final_response()).output_text == REPLY
    tool = {
        "type": "function",
        "name": "lookup",
        "parameters": {"type": "object", "properties": {"city": {"type": "string"}}},
        "strict": False,
    }
    with client.responses.stream(
        model="gemini-test", input="lookup", tools=[tool]
    ) as stream:
        for _ in stream:
            pass
        called = validated(stream.get_final_response())
    call = next(item for item in called.output if item.type == "function_call")
    answered = client.responses.create(
        model="gemini-test",
        previous_response_id=called.id,
        input=[
            {"type": "function_call_output", "call_id": call.call_id, "output": "found"}
        ],
    )
    assert answered.output_text == REPLY
    client.responses.delete(called.id)  # the SDK returns no body for deletes
    expect(openai.NotFoundError, lambda: client.responses.retrieve(called.id))
    expect(
        openai.BadRequestError,
        lambda: client.responses.create(
            model="gemini-test", input="hello", tools=[{"type": "web_search"}]
        ),
        lambda error: "unsupported" in str(error),
    )


def check_images(client):
    generated = validated(
        client.images.generate(model="gemini-canvas", prompt="draw", n=2)
    )
    assert len(generated.data) == 2 and generated.usage.total_tokens == 30
    assert base64.b64decode(generated.data[0].b64_json) == png_fixture()
    for images in [
        ("input.png", png_fixture(), "image/png"),
        [
            ("one.png", png_fixture(), "image/png"),
            ("two.png", png_fixture(), "image/png"),
        ],
    ]:
        edited = validated(
            client.images.edit(
                model="gemini-canvas",
                prompt="edit",
                image=images,
                output_format="jpeg",
                output_compression=85,
            )
        )
        assert edited.output_format == "jpeg"
        assert base64.b64decode(edited.data[0].b64_json).startswith(b"\xff\xd8")
    data_url = "data:image/png;base64," + base64.b64encode(png_fixture()).decode()
    edited = validated(
        client.post(
            "/images/edits",
            cast_to=openai.types.ImagesResponse,
            body={
                "model": "gemini-canvas",
                "prompt": "edit",
                "images": [{"image_url": data_url}],
            },
        )
    )
    assert base64.b64decode(edited.data[0].b64_json) == png_fixture()
    expect(
        openai.BadRequestError,
        lambda: client.images.generate(model="gemini-test", prompt="draw"),
        lambda error: error.param == "model",
    )


def check_messages(client):
    # Image generation models only serve the OpenAI Images API.
    assert [m.id for m in client.models.list().data] == ["gemini-test"]
    messages = [{"role": "user", "content": "hello"}]
    result = validated(
        client.messages.create(model="gemini-test", max_tokens=1024, messages=messages)
    )
    assert result.content[0].text == REPLY
    counted = client.messages.count_tokens(model="gemini-test", messages=messages)
    assert validated(counted).input_tokens == 17
    with client.messages.stream(
        model="gemini-test", max_tokens=1024, messages=messages
    ) as stream:
        assert "".join(stream.text_stream) == REPLY
        validated(stream.get_final_message())
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
        model="gemini-test", max_tokens=1024, messages=messages, tools=tools
    ) as stream:
        for _ in stream:
            pass
        called = validated(stream.get_final_message())
    assert called.stop_reason == "tool_use"
    call = next(block for block in called.content if block.type == "tool_use")
    messages += [
        {
            "role": "assistant",
            "content": [b.model_dump(exclude_none=True) for b in called.content],
        },
        {
            "role": "user",
            "content": [
                {"type": "tool_result", "tool_use_id": call.id, "content": "found"}
            ],
        },
    ]
    answered = client.messages.create(
        model="gemini-test", max_tokens=1024, messages=messages, tools=tools
    )
    assert answered.content[0].text == REPLY
    expect(
        anthropic.BadRequestError,
        lambda: client.messages.create(
            model="gemini-test",
            max_tokens=1024,
            messages=messages[:1],
            tools=[{"type": "web_search_20250305", "name": "web_search"}],
        ),
        lambda error: "unsupported" in str(error),
    )


def check():
    upstream = ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    try:
        with tempfile.TemporaryDirectory(prefix="agy2api-sdk-") as directory:
            root = pathlib.Path(directory)
            binary = build(root, f"http://127.0.0.1:{upstream.server_port}")
            with socket.socket() as sock:
                sock.bind(("127.0.0.1", 0))
                port = sock.getsockname()[1]
            config, ids = write_accounts(root, port)
            env = dict(os.environ)
            for name in ("AGY2API_LISTEN", "AGY2API_DATA_DIR", "AGY2API_PROXY_URL"):
                env.pop(name, None)

            def cli(command):
                return subprocess.run(
                    [str(binary), command, "--config", str(config)],
                    env=env,
                    capture_output=True,
                    text=True,
                    timeout=15,
                    check=True,
                ).stdout

            base = f"http://127.0.0.1:{port}"
            with (
                (root / "service.log").open("w+") as log,
                serving(binary, config, env, base, log),
            ):
                check_accounts(base, cli, ids)
                with openai.OpenAI(
                    api_key="fixture-key", base_url=base + "/v1", max_retries=0
                ) as client:
                    check_responses(client)
                    check_images(client)
                with anthropic.Anthropic(
                    api_key="fixture-key", base_url=base, max_retries=0
                ) as client:
                    check_messages(client)
            print(
                f"SDK checks passed: openai={openai.__version__}, anthropic={anthropic.__version__}"
            )
    finally:
        upstream.shutdown()
        upstream.server_close()


if __name__ == "__main__":
    argparse.ArgumentParser(description=__doc__).parse_args()
    check()
