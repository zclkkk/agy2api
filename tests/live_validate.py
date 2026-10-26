"""Opt-in real-account acceptance tests. Consumes the configured subscription.

Uses an isolated state directory and never writes credentials or opaque replay
payloads to the report. Requires tests/live-requirements.txt. No test runs on import.
"""

import argparse
import base64
import concurrent.futures
import datetime
import hashlib
import io
import json
import os
import shutil
import socket
import sqlite3
import struct
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
import wave
import zlib
from pathlib import Path

import anthropic
import openai


class PartialCoverage(Exception):
    """A supported operation with a verified protocol-information limitation."""


def png_fixture():
    def chunk(kind, value):
        return (
            struct.pack("!I", len(value))
            + kind
            + value
            + struct.pack("!I", zlib.crc32(kind + value))
        )

    rows = b"".join(b"\0" + b"\xff\0\0" * 32 + b"\0\0\xff" * 32 for _ in range(64))
    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", struct.pack("!IIBBBBB", 64, 64, 8, 2, 0, 0, 0))
        + chunk(b"IDAT", zlib.compress(rows))
        + chunk(b"IEND", b"")
    )


def pdf_fixture():
    stream = b"BT /F1 16 Tf 50 750 Td (Verification code: AGY_PDF_7315) Tj ET"
    objects = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
        b"<< /Length "
        + str(len(stream)).encode()
        + b" >>\nstream\n"
        + stream
        + b"\nendstream",
    ]
    data = bytearray(b"%PDF-1.4\n")
    offsets = [0]
    for i, value in enumerate(objects, 1):
        offsets.append(len(data))
        data += f"{i} 0 obj\n".encode() + value + b"\nendobj\n"
    start = len(data)
    data += f"xref\n0 {len(offsets)}\n0000000000 65535 f \n".encode()
    for offset in offsets[1:]:
        data += f"{offset:010} 00000 n \n".encode()
    data += f"trailer\n<< /Size {len(offsets)} /Root 1 0 R >>\nstartxref\n{start}\n%%EOF\n".encode()
    return bytes(data)


class Live:
    def __init__(self, config, binary, report):
        self.source = json.loads(config.read_text())
        self.source_data = Path(
            os.environ.get("AGY2API_DATA_DIR", self.source.get("data_dir", "data"))
        ).resolve()
        self.binary, self.report_path = binary.resolve(), report
        self.tmp = tempfile.TemporaryDirectory(prefix="agy2api-live-")
        self.root = Path(self.tmp.name)
        data = self.root / "data"
        data.mkdir(mode=0o700)
        shutil.copyfile(
            self.source_data / "credentials.json", data / "credentials.json"
        )
        (data / "credentials.json").chmod(0o600)
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            self.port = sock.getsockname()[1]
        self.base = f"http://127.0.0.1:{self.port}"
        self.key = "agy2api-live-local-key"
        cfg = dict(
            self.source,
            listen=f"127.0.0.1:{self.port}",
            data_dir=str(data),
            api_key=self.key,
        )
        (self.root / "config.json").write_text(json.dumps(cfg))
        self.env = dict(os.environ)
        for name in ("AGY2API_DATA_DIR", "AGY2API_LISTEN", "AGY2API_API_KEY"):
            self.env.pop(name, None)
        self.log = (self.root / "service.log").open("w+")
        self.process = None
        self.results = []
        self.lock = threading.Lock()
        self.started = datetime.datetime.now(datetime.timezone.utc).isoformat()
        self.start()
        self.o = openai.OpenAI(
            api_key=self.key, base_url=self.base + "/v1", max_retries=0, timeout=120
        )
        self.a = anthropic.Anthropic(
            api_key=self.key, base_url=self.base, max_retries=0, timeout=120
        )

    def start(self):
        self.process = subprocess.Popen(
            [str(self.binary), "serve", "--config", str(self.root / "config.json")],
            env=self.env,
            stderr=self.log,
        )
        deadline = time.monotonic() + 10
        while True:
            if self.process.poll() is not None:
                raise RuntimeError("verification service failed to start")
            try:
                with urllib.request.urlopen(self.base + "/healthz", timeout=1):
                    return
            except urllib.error.URLError:
                if time.monotonic() > deadline:
                    raise
                time.sleep(0.02)

    def restart(self):
        self.process.terminate()
        self.process.wait(timeout=10)
        self.start()

    def close(self):
        self.o.close()
        self.a.close()
        if self.process:
            self.process.terminate()
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()
        self.log.close()
        self.tmp.cleanup()

    def write_report(self):
        self.report_path.parent.mkdir(parents=True, exist_ok=True)
        self.report_path.write_text(
            json.dumps(
                {
                    "started_at": self.started,
                    "sdk_versions": {
                        "openai": openai.__version__,
                        "anthropic": anthropic.__version__,
                    },
                    "results": self.results,
                },
                ensure_ascii=False,
                indent=2,
            )
            + "\n"
        )

    def run_case(self, name, fn):
        start = time.monotonic()
        try:
            summary = fn() or {}
            result = {
                "name": name,
                "status": "pass",
                "seconds": round(time.monotonic() - start, 2),
                "summary": summary,
            }
        except Exception as error:
            status = (
                "partial"
                if isinstance(error, PartialCoverage)
                else (
                    "blocked"
                    if any(
                        code in str(error)
                        for code in (
                            "MODEL_CAPACITY_EXHAUSTED",
                            "RATE_LIMIT_EXCEEDED",
                            "QUOTA_EXHAUSTED",
                        )
                    )
                    else "fail"
                )
            )
            result = {
                "name": name,
                "status": status,
                "seconds": round(time.monotonic() - start, 2),
                "error_type": type(error).__name__,
                "error": str(error)[:800],
            }
            if getattr(error, "response", None) is not None:
                result["retry_after"] = error.response.headers.get("retry-after")
        with self.lock:
            self.results.append(result)
            self.write_report()
            print(json.dumps(result, ensure_ascii=False), flush=True)

    def request(self, proto, model, prompt, **kwargs):
        if proto == "responses":
            return self.o.responses.create(
                model=model, input=prompt, store=False, **kwargs
            )
        messages = (
            prompt
            if isinstance(prompt, list)
            else [{"role": "user", "content": prompt}]
        )
        return self.a.messages.create(
            model=model,
            max_tokens=kwargs.pop("max_tokens", 4096),
            messages=messages,
            **kwargs,
        )

    @staticmethod
    def text(result):
        if hasattr(result, "output_text"):
            return result.output_text
        return "".join(b.text for b in result.content if b.type == "text")

    @staticmethod
    def details(result):
        return {
            "usage": result.usage.model_dump(exclude_none=True),
            "text": Live.text(result)[:160],
        }

    def basic(self, proto, model):
        r = self.request(proto, model, "Reply with exactly AGY_OK.")
        assert "AGY_OK" in self.text(r), "expected marker absent"
        type(r).model_validate(r.model_dump())
        return self.details(r)

    def streaming(self, proto, model):
        if proto == "responses":
            with self.o.responses.stream(
                model=model, input="Reply with exactly AGY_STREAM_OK.", store=False
            ) as stream:
                events = list(stream)
                r = stream.get_final_response()
            sequences = [e.sequence_number for e in events]
            assert sequences == list(range(len(sequences))), (
                "SSE sequence numbers are not consecutive"
            )
            assert events[-1].type == "response.completed"
        else:
            with self.a.messages.stream(
                model=model,
                max_tokens=4096,
                messages=[
                    {"role": "user", "content": "Reply with exactly AGY_STREAM_OK."}
                ],
            ) as stream:
                events = list(stream)
                r = stream.get_final_message()
            assert events[-1].type == "message_stop"
        assert "AGY_STREAM_OK" in self.text(r), "stream aggregation lost text"
        type(r).model_validate(r.model_dump())
        return dict(self.details(r), event_count=len(events))

    def tools(self, proto, model, parallel=False, thinking=False):
        schema = {
            "type": "object",
            "properties": {"a": {"type": "integer"}, "b": {"type": "integer"}},
            "required": ["a", "b"],
        }
        prompt = "Use the add tool for a=17 and b=25. After the result, report the result as ANSWER=42."
        if parallel:
            prompt = "Call add TWICE in parallel in this turn: first a=17,b=25 and second a=3,b=8. Do not combine them. After both tool results, report their sum as ANSWER=53."
        extra = {}
        if proto == "responses":
            tool = {
                "type": "function",
                "name": "add",
                "description": "Add two integers.",
                "parameters": schema,
                "strict": False,
            }
            if thinking:
                extra["reasoning"] = {"effort": "medium", "summary": "auto"}
            with self.o.responses.stream(
                model=model,
                input=prompt,
                tools=[tool],
                tool_choice="auto" if thinking else "required",
                store=False,
                **extra,
            ) as stream:
                list(stream)
                first = stream.get_final_response()
            calls = [item for item in first.output if item.type == "function_call"]
            assert len(calls) == (2 if parallel else 1), (
                f"expected tool calls, got {len(calls)}"
            )
            history = [{"role": "user", "content": prompt}] + [
                item.model_dump(exclude_none=True) for item in first.output
            ]
            for call in reversed(calls):
                args = json.loads(call.arguments)
                assert isinstance(args["a"], int) and isinstance(args["b"], int)
                history.append(
                    {
                        "type": "function_call_output",
                        "call_id": call.call_id,
                        "output": json.dumps({"result": args["a"] + args["b"]}),
                    }
                )
            second = self.o.responses.create(
                model=model, input=history, tools=[tool], store=False, **extra
            )
            thoughts = sum(item.type == "reasoning" for item in first.output)
        else:
            tool = {
                "name": "add",
                "description": "Add two integers.",
                "input_schema": schema,
            }
            if thinking:
                extra["thinking"] = {"type": "enabled", "budget_tokens": 1024}
            choice = {"type": "any"} if not thinking else {"type": "auto"}
            with self.a.messages.stream(
                model=model,
                max_tokens=4096,
                messages=[{"role": "user", "content": prompt}],
                tools=[tool],
                tool_choice=choice,
                **extra,
            ) as stream:
                list(stream)
                first = stream.get_final_message()
            calls = [b for b in first.content if b.type == "tool_use"]
            assert len(calls) == (2 if parallel else 1), (
                f"expected tool calls, got {len(calls)}"
            )
            outputs = [
                {
                    "type": "tool_result",
                    "tool_use_id": call.id,
                    "content": json.dumps(
                        {"result": call.input["a"] + call.input["b"]}
                    ),
                }
                for call in reversed(calls)
            ]
            history = [
                {"role": "user", "content": prompt},
                {
                    "role": "assistant",
                    "content": [b.model_dump(exclude_none=True) for b in first.content],
                },
                {"role": "user", "content": outputs},
            ]
            second = self.a.messages.create(
                model=model, max_tokens=4096, messages=history, tools=[tool], **extra
            )
            thoughts = sum(b.type == "thinking" for b in first.content)
        assert ("53" if parallel else "42") in self.text(second), (
            "tool result was not used"
        )
        signed_tools = 0
        with sqlite3.connect(self.root / "data" / "state.db") as db:
            for call in calls:
                call_id = call.call_id if proto == "responses" else call.id
                row = db.execute(
                    "SELECT data FROM entries WHERE kind='tool' AND id=?", (call_id,)
                ).fetchone()
                signed_tools += bool(row and json.loads(row[0]).get("signature"))
        # Some Gemini versions expose opaque thinking only on function-call parts.
        # Their visible reasoning block may be absent; the signature still must
        # survive the actual tool-result round trip, which the second call checks.
        if thinking:
            assert thoughts > 0 or signed_tools > 0, (
                "no visible thinking or signed tool context was preserved"
            )
        return dict(
            self.details(second),
            calls=len(calls),
            thinking_blocks=thoughts,
            signed_tool_calls=signed_tools,
        )

    def thinking(self, proto, model):
        prompt = "Compute 137*249 carefully, and answer with the result."
        if proto == "responses":
            with self.o.responses.stream(
                model=model,
                input=prompt,
                reasoning={"effort": "medium", "summary": "auto"},
                store=False,
            ) as stream:
                list(stream)
                first = stream.get_final_response()
            thoughts = [v for v in first.output if v.type == "reasoning"]
            assert thoughts and all(v.encrypted_content for v in thoughts), (
                "no replayable reasoning returned"
            )
            history = (
                [{"role": "user", "content": prompt}]
                + [v.model_dump(exclude_none=True) for v in first.output]
                + [{"role": "user", "content": "Subtract 100 from that result."}]
            )
            second = self.o.responses.create(
                model=model, input=history, reasoning={"effort": "medium"}, store=False
            )
        else:
            with self.a.messages.stream(
                model=model,
                max_tokens=4096,
                messages=[{"role": "user", "content": prompt}],
                thinking={"type": "enabled", "budget_tokens": 1024},
            ) as stream:
                list(stream)
                first = stream.get_final_message()
            thoughts = [v for v in first.content if v.type == "thinking"]
            assert thoughts and all(v.signature for v in thoughts), (
                "no replayable thinking returned"
            )
            history = [
                {"role": "user", "content": prompt},
                {
                    "role": "assistant",
                    "content": [v.model_dump(exclude_none=True) for v in first.content],
                },
                {"role": "user", "content": "Subtract 100 from that result."},
            ]
            second = self.a.messages.create(
                model=model,
                max_tokens=4096,
                messages=history,
                thinking={"type": "enabled", "budget_tokens": 1024},
            )
        assert "34113" in self.text(first).replace(",", ""), (
            "wrong first arithmetic result"
        )
        assert "34013" in self.text(second).replace(",", ""), (
            "reasoning follow-up failed"
        )
        return dict(self.details(second), thinking_blocks=len(thoughts))

    def structured(self, proto, model):
        schema = {
            "type": "object",
            "properties": {"answer": {"type": "integer"}, "ok": {"type": "boolean"}},
            "required": ["answer", "ok"],
        }
        if proto == "responses":
            r = self.request(
                proto,
                model,
                "Return answer=42 and ok=true as JSON.",
                text={
                    "format": {
                        "type": "json_schema",
                        "name": "result",
                        "schema": schema,
                        "strict": False,
                    }
                },
            )
        else:
            r = self.request(
                proto,
                model,
                "Return answer=42 and ok=true as JSON.",
                output_config={"format": {"type": "json_schema", "schema": schema}},
            )
        assert json.loads(self.text(r)) == {"answer": 42, "ok": True}
        return self.details(r)

    def media(self, proto, model, kind):
        is_image = kind == "image"
        data = png_fixture() if is_image else pdf_fixture()
        encoded = base64.b64encode(data).decode()
        mime = "image/png" if is_image else "application/pdf"
        prompt = (
            "Name the left half color and the right half color in English."
            if is_image
            else "Return the verification code written inside this PDF."
        )
        if proto == "responses":
            part = (
                {"type": "input_image", "image_url": f"data:{mime};base64,{encoded}"}
                if is_image
                else {
                    "type": "input_file",
                    "filename": "test.pdf",
                    "file_data": f"data:{mime};base64,{encoded}",
                }
            )
            r = self.request(
                proto,
                model,
                [
                    {
                        "role": "user",
                        "content": [{"type": "input_text", "text": prompt}, part],
                    }
                ],
            )
        else:
            part = {
                "type": "image" if is_image else "document",
                "source": {"type": "base64", "media_type": mime, "data": encoded},
            }
            r = self.request(
                proto,
                model,
                [{"role": "user", "content": [{"type": "text", "text": prompt}, part]}],
            )
        text = self.text(r).lower()
        assert (
            ("red" in text and "blue" in text) if is_image else "agy_pdf_7315" in text
        ), "attachment content was not understood"
        return self.details(r)

    def remote_image(self):
        r = self.request(
            "responses",
            "gemini-3-flash",
            [
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "input_text",
                            "text": "What company name is shown in this image?",
                        },
                        {
                            "type": "input_image",
                            "image_url": "https://www.google.com/images/branding/googlelogo/2x/googlelogo_color_92x30dp.png",
                        },
                    ],
                }
            ],
        )
        assert "google" in self.text(r).lower()
        return self.details(r)

    def search(self, proto):
        model = "gemini-3.5-flash-lite"
        prompt = "Search the web for the official Python documentation for the json module. Give its URL and one sentence describing json.loads."
        tools = (
            [{"type": "web_search"}]
            if proto == "responses"
            else [{"type": "web_search_20250305", "name": "web_search"}]
        )
        r = self.request(proto, model, prompt, tools=tools)
        if proto == "responses":
            citations = [
                a
                for item in r.output
                if item.type == "message"
                for p in item.content
                if p.type == "output_text"
                for a in p.annotations
            ]
        else:
            citations = [
                c for b in r.content if b.type == "text" for c in (b.citations or [])
            ]
        assert citations, "no search citations returned"
        type(r).model_validate(r.model_dump())
        # Search redirect links are replaced by the pages they point to.
        urls = [c.url for c in citations]
        resolved = [u for u in urls if "grounding-api-redirect" not in u]
        assert resolved, f"citation links were not resolved: {urls}"
        return dict(
            self.details(r),
            citation_count=len(citations),
            resolved_links=f"{len(resolved)}/{len(urls)}",
            sample_url=resolved[0],
        )

    def count(self, proto, model):
        if proto == "messages":
            r = self.a.messages.count_tokens(
                model=model,
                messages=[
                    {"role": "user", "content": "Count these input tokens accurately."}
                ],
            )
            count = r.input_tokens
        else:
            req = urllib.request.Request(
                self.base + "/v1/responses/input_tokens",
                data=json.dumps(
                    {"model": model, "input": "Count these input tokens accurately."}
                ).encode(),
                headers={
                    "Authorization": "Bearer " + self.key,
                    "Content-Type": "application/json",
                },
            )
            with urllib.request.urlopen(req, timeout=45) as response:
                count = json.load(response)["input_tokens"]
        assert count > 0
        return {"input_tokens": count}

    def persistence(self):
        first = self.o.responses.create(
            model="gemini-3-flash",
            input="Remember this code word: ORCHID7315. Acknowledge briefly.",
        )
        self.restart()
        assert self.o.responses.retrieve(first.id).id == first.id
        second = self.o.responses.create(
            model="gemini-3-flash",
            previous_response_id=first.id,
            input="What was the code word?",
        )
        assert "ORCHID7315" in second.output_text
        self.o.responses.delete(first.id)
        self.o.responses.delete(second.id)
        try:
            self.o.responses.retrieve(first.id)
        except openai.NotFoundError:
            pass
        else:
            raise AssertionError("response survived deletion")
        return {"restart_recovered": True, "deleted": True}

    def limits(self, proto, model):
        args = {"max_output_tokens": 12} if proto == "responses" else {"max_tokens": 12}
        r = self.request(
            proto, model, "Write all integers from 1 to 200 with no omissions.", **args
        )
        if proto == "responses":
            assert (
                r.status == "incomplete"
                and r.incomplete_details.reason == "max_output_tokens"
            ), "output limit not reflected"
        else:
            assert r.stop_reason == "max_tokens", "output limit not reflected"
        return self.details(r)

    def image_generation(self):
        r = self.o.responses.create(
            model="gemini-3.1-flash-image",
            input="Generate a simple image of a red circle on a plain white background.",
            store=False,
            timeout=180,
        )
        images = [item for item in r.output if item.type == "image_generation_call"]
        assert images and images[0].result, "no generated image returned"
        data = base64.b64decode(images[0].result)
        assert len(data) > 100
        type(r).model_validate(r.model_dump())
        return {
            "image_count": len(images),
            "image_bytes": len(data),
            "image_sha256": hashlib.sha256(data).hexdigest(),
            "usage": r.usage.model_dump(),
        }

    def image_edit_roundtrip(self):
        def generate(**kwargs):
            for attempt in range(3):
                try:
                    return self.o.responses.create(**kwargs)
                except openai.RateLimitError as error:
                    delay = error.response.headers.get("retry-after")
                    delay = float(delay) if delay and delay.isdigit() else 60
                    if attempt == 2 or delay > 180:
                        raise
                    print(
                        json.dumps(
                            {
                                "name": "media/image_edit_roundtrip",
                                "waiting_for_upstream_seconds": delay,
                            }
                        ),
                        flush=True,
                    )
                    remaining = max(delay, 1)
                    while remaining > 0:
                        step = min(remaining, 30)
                        time.sleep(step)
                        remaining -= step

        original = (
            "Generate a simple image of one red circle on a plain white background."
        )
        first = generate(
            model="gemini-3.1-flash-image", input=original, store=False, timeout=180
        )
        assert any(v.type == "image_generation_call" for v in first.output)
        history = (
            [{"role": "user", "content": original}]
            + [v.model_dump(exclude_none=True) for v in first.output]
            + [
                {
                    "role": "user",
                    "content": "Edit that image: change the circle to blue and keep the white background.",
                }
            ]
        )
        second = generate(
            model="gemini-3.1-flash-image", input=history, store=False, timeout=180
        )
        images = [v for v in second.output if v.type == "image_generation_call"]
        assert images and images[0].result
        checked = self.o.responses.create(
            model="gemini-3-flash",
            input=[
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "input_text",
                            "text": "What color is the circle in this image? Answer in English.",
                        },
                        {
                            "type": "input_image",
                            "image_url": "data:image/png;base64," + images[0].result,
                        },
                    ],
                }
            ],
            store=False,
        )
        assert "blue" in checked.output_text.lower(), (
            "edited image did not reflect the requested change"
        )
        return {
            "image_edit_roundtrip": True,
            "image_bytes": len(base64.b64decode(images[0].result)),
            "vision_check": checked.output_text[:160],
        }

    def cancellation(self):
        with self.o.responses.stream(
            model="gemini-3-flash",
            input="Write a long essay of at least 2000 words about numbers.",
            store=False,
        ) as stream:
            for event in stream:
                if event.type == "response.output_text.delta":
                    break
        return self.basic("responses", "gemini-3-flash")

    def negative(self):
        cases = [
            (
                "/v1/responses",
                {"model": "gemini-3-flash", "input": "hello", "background": True},
                400,
            ),
            ("/v1/responses", {"model": "missing-model", "input": "hello"}, 400),
            (
                "/v1/responses",
                {
                    "model": "gemini-3-flash",
                    "input": [
                        {
                            "role": "user",
                            "content": [
                                {"type": "input_image", "file_id": "unsupported"}
                            ],
                        }
                    ],
                },
                400,
            ),
            (
                "/v1/responses",
                {"model": "gemini-3-flash", "input": "hello", "conversation": "conv_1"},
                400,
            ),
            (
                "/v1/responses",
                {
                    "model": "gemini-3-flash",
                    "input": [{"type": "item_reference", "id": "msg_1"}],
                },
                400,
            ),
            ("/v1/chat/completions", {}, 404),
        ]
        for path, payload, expected in cases:
            req = urllib.request.Request(
                self.base + path,
                data=json.dumps(payload).encode(),
                headers={
                    "Authorization": "Bearer " + self.key,
                    "Content-Type": "application/json",
                },
            )
            try:
                urllib.request.urlopen(req, timeout=20)
            except urllib.error.HTTPError as e:
                assert e.code == expected, f"{path}: {e.code}"
            else:
                raise AssertionError(f"{path} accepted unsupported input")
        try:
            urllib.request.urlopen(self.base + "/v1/models", timeout=10)
        except urllib.error.HTTPError as e:
            assert e.code == 401
        else:
            raise AssertionError("API key was not enforced")
        return {"expected_rejections": len(cases) + 1}

    def system(self, proto):
        options = (
            {"instructions": "Reply only with SYSTEM_PRIORITY_OK."}
            if proto == "responses"
            else {"system": "Reply only with SYSTEM_PRIORITY_OK."}
        )
        r = self.request(proto, "gemini-3-flash", "Tell me a story instead.", **options)
        assert self.text(r).strip() == "SYSTEM_PRIORITY_OK"
        return self.details(r)

    def long_context(self, proto):
        lines = [
            f"Record {i:04}: The ordinary inventory item is a blue wooden box."
            for i in range(3000)
        ]
        lines[1731] = "Unique verification code: AGY_NEEDLE_920317"
        prompt = (
            "\n".join(lines)
            + "\nReturn the unique verification code from the records, and nothing else."
        )
        model = "gemini-3-flash" if proto == "responses" else "gemini-3.8-flash-medium"
        r = self.request(proto, model, prompt)
        assert "AGY_NEEDLE_920317" in self.text(r)
        assert r.usage.input_tokens > 10000
        return dict(self.details(r), input_bytes=len(prompt.encode()))

    def av_file(self, kind):
        if kind == "audio":
            buffer = io.BytesIO()
            with wave.open(buffer, "wb") as output:
                output.setnchannels(1)
                output.setsampwidth(2)
                output.setframerate(16000)
                output.writeframes(b"\0" * 64000)
            data, mime, filename = buffer.getvalue(), "audio/wav", "silence.wav"
            prompt = "Does this audio contain speech or is it silent? Answer SILENT if it is silent."
        else:
            import imageio_ffmpeg

            filename, mime = "colors.mp4", "video/mp4"
            path = self.root / filename
            subprocess.run(
                [
                    imageio_ffmpeg.get_ffmpeg_exe(),
                    "-hide_banner",
                    "-loglevel",
                    "error",
                    "-f",
                    "lavfi",
                    "-i",
                    "color=c=red:s=128x128:r=5:d=1",
                    "-f",
                    "lavfi",
                    "-i",
                    "color=c=blue:s=128x128:r=5:d=1",
                    "-filter_complex",
                    "[0:v][1:v]concat=n=2:v=1:a=0",
                    "-c:v",
                    "libx264",
                    "-pix_fmt",
                    "yuv420p",
                    "-movflags",
                    "+faststart",
                    "-y",
                    str(path),
                ],
                check=True,
                timeout=30,
            )
            data = path.read_bytes()
            prompt = "What are the two background colors in this video, in temporal order? Name the first and second color in English."
        part = {
            "type": "input_file",
            "filename": filename,
            "file_data": f"data:{mime};base64," + base64.b64encode(data).decode(),
        }
        r = self.request(
            "responses",
            "gemini-3-flash",
            [
                {
                    "role": "user",
                    "content": [{"type": "input_text", "text": prompt}, part],
                }
            ],
        )
        text = self.text(r).lower()
        assert (
            ("silent" in text or "silence" in text)
            if kind == "audio"
            else (
                "red" in text
                and "blue" in text
                and text.index("red") < text.index("blue")
            )
        )
        return dict(self.details(r), file_bytes=len(data))

    def search_stream(self, proto):
        prompt = "搜索 Python json 模块的官方文档，用中文介绍 json.loads，并引用来源。"
        if proto == "responses":
            with self.o.responses.stream(
                model="gemini-3.5-flash-lite",
                input=prompt,
                tools=[{"type": "web_search"}],
                store=False,
            ) as stream:
                events = list(stream)
                r = stream.get_final_response()
            count = 0
            for item in r.output:
                if item.type != "message":
                    continue
                for part in item.content:
                    if part.type != "output_text":
                        continue
                    for citation in part.annotations:
                        assert (
                            0
                            <= citation.start_index
                            <= citation.end_index
                            <= len(part.text)
                        )
                        count += 1
        else:
            with self.a.messages.stream(
                model="gemini-3.5-flash-lite",
                max_tokens=4096,
                messages=[{"role": "user", "content": prompt}],
                tools=[{"type": "web_search_20250305", "name": "web_search"}],
            ) as stream:
                events = list(stream)
                r = stream.get_final_message()
            count = sum(len(b.citations or []) for b in r.content if b.type == "text")
        type(r).model_validate(r.model_dump())
        assert count > 0
        return dict(self.details(r), citations=count, events=len(events))

    def schema_tool(self, proto):
        schema = {
            "type": "object",
            "properties": {
                "title": {"type": "string"},
                "format": {"type": "string"},
                "default": {"type": "string"},
            },
            "required": ["title", "format", "default"],
        }
        prompt = 'Call inspect with title="上海", format="json", default="KEEP". Do not answer directly.'
        if proto == "responses":
            r = self.request(
                proto,
                "gemini-3-flash",
                prompt,
                tools=[{"type": "function", "name": "inspect", "parameters": schema}],
                tool_choice="required",
            )
            calls = [c for c in r.output if c.type == "function_call"]
            assert len(calls) == 1
            args = json.loads(calls[0].arguments)
        else:
            r = self.request(
                proto,
                "gemini-3.8-flash-medium",
                prompt,
                tools=[{"name": "inspect", "input_schema": schema}],
                tool_choice={"type": "tool", "name": "inspect"},
            )
            calls = [c for c in r.content if c.type == "tool_use"]
            assert len(calls) == 1
            args = calls[0].input
        assert args == {"title": "上海", "format": "json", "default": "KEEP"}
        return {"tool_parameters_preserved": True}

    def tool_image(self, proto, model):
        prompt = "Use the get_chart tool. After receiving its image, name the left half color and the right half color in English."
        schema = {"type": "object", "properties": {}}
        encoded = base64.b64encode(png_fixture()).decode()
        if proto == "responses":
            tools = [
                {
                    "type": "function",
                    "name": "get_chart",
                    "description": "Get the chart image.",
                    "parameters": schema,
                }
            ]
            first = self.o.responses.create(
                model=model,
                input=prompt,
                tools=tools,
                tool_choice="required",
                store=False,
            )
            call = next(v for v in first.output if v.type == "function_call")
            output = [
                {"type": "input_text", "text": "Here is the chart."},
                {
                    "type": "input_image",
                    "image_url": "data:image/png;base64," + encoded,
                },
            ]
            history = (
                [{"role": "user", "content": prompt}]
                + [v.model_dump(exclude_none=True) for v in first.output]
                + [
                    {
                        "type": "function_call_output",
                        "call_id": call.call_id,
                        "output": output,
                    }
                ]
            )
            second = self.o.responses.create(
                model=model, input=history, tools=tools, store=False
            )
        else:
            tools = [
                {
                    "name": "get_chart",
                    "description": "Get the chart image.",
                    "input_schema": schema,
                }
            ]
            first = self.a.messages.create(
                model=model,
                max_tokens=4096,
                messages=[{"role": "user", "content": prompt}],
                tools=tools,
                tool_choice={"type": "any"},
            )
            call = next(v for v in first.content if v.type == "tool_use")
            output = [
                {"type": "text", "text": "Here is the chart."},
                {
                    "type": "image",
                    "source": {
                        "type": "base64",
                        "media_type": "image/png",
                        "data": encoded,
                    },
                },
            ]
            history = [
                {"role": "user", "content": prompt},
                {
                    "role": "assistant",
                    "content": [v.model_dump(exclude_none=True) for v in first.content],
                },
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "tool_result",
                            "tool_use_id": call.id,
                            "content": output,
                        }
                    ],
                },
            ]
            second = self.a.messages.create(
                model=model, max_tokens=4096, messages=history, tools=tools
            )
        text = self.text(second).lower()
        assert "red" in text and "blue" in text, (
            "image in tool result was not understood"
        )
        return self.details(second)

    def stop_sequence(self, model):
        r = self.a.messages.create(
            model=model,
            max_tokens=1024,
            messages=[
                {
                    "role": "user",
                    "content": "Output exactly this literal text: ALPHA END OMEGA",
                }
            ],
            stop_sequences=[" END"],
        )
        assert self.text(r).strip() == "ALPHA", (
            "native stop sequence failed to truncate the answer"
        )
        if r.stop_reason != "stop_sequence" or r.stop_sequence != " END":
            raise PartialCoverage(
                "Text truncation works, but Antigravity reports only STOP without the matched sequence; the response currently has end_turn/null instead of stop_sequence/the matched value."
            )
        return self.details(r)

    def reasoning_tamper(self):
        prompt = "Compute 137*249 carefully."
        r = self.o.responses.create(
            model="gemini-3-flash",
            input=prompt,
            reasoning={"effort": "medium", "summary": "auto"},
            store=False,
        )
        history = [{"role": "user", "content": prompt}] + [
            v.model_dump(exclude_none=True) for v in r.output
        ]
        item = next(v for v in history if v.get("type") == "reasoning")
        value = item["encrypted_content"]
        position = len(value) // 2
        item["encrypted_content"] = (
            value[:position]
            + ("A" if value[position] != "A" else "B")
            + value[position + 1 :]
        )
        history.append({"role": "user", "content": "Continue."})
        self.o.responses.create(model="gemini-3-flash", input=history, store=False)
        self.log.flush()
        self.log.seek(0)
        assert "dropped a reasoning item" in self.log.read(), "tampering not detected"
        try:
            self.o.responses.retrieve(r.id)
        except openai.NotFoundError:
            pass
        else:
            raise AssertionError("store=false response was persisted")
        return {"tampered_reasoning_dropped": True, "store_false_verified": True}

    def refresh(self):
        self.process.terminate()
        self.process.wait(timeout=10)
        path = self.root / "data" / "credentials.json"
        creds = json.loads(path.read_text())
        creds["token"]["expiry"] = "2000-01-01T00:00:00Z"
        path.write_text(json.dumps(creds))
        path.chmod(0o600)
        self.start()
        assert self.o.models.list().data
        updated = json.loads(path.read_text())
        expiry = datetime.datetime.fromisoformat(
            updated["token"]["expiry"].replace("Z", "+00:00")
        )
        assert expiry > datetime.datetime.now(datetime.timezone.utc)
        assert path.stat().st_mode & 0o777 == 0o600
        return {"real_refresh_succeeded": True, "credentials_saved_privately": True}

    def concurrency(self):
        workers = int(self.source.get("max_concurrent_requests", 4)) + 2
        barrier = threading.Barrier(workers)

        def request():
            barrier.wait()
            r = self.o.responses.create(
                model="gemini-3-flash",
                input="Reply exactly CONCURRENT_OK.",
                store=False,
            )
            assert "CONCURRENT_OK" in r.output_text
            return "accepted"

        with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:
            results = list(pool.map(lambda _: request(), range(workers)))
        assert results.count("accepted") == workers, results
        return {"burst": workers, "accepted": results.count("accepted"), "queued": True}

    def raw(self, path, payload):
        req = urllib.request.Request(
            self.base + path,
            data=json.dumps(payload).encode(),
            headers={
                "Authorization": "Bearer " + self.key,
                "Content-Type": "application/json",
                "anthropic-version": "2023-06-01",
            },
        )
        try:
            with urllib.request.urlopen(req, timeout=180) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            raise RuntimeError(f"HTTP {error.code}: {error.read()[:400]!r}") from None

    # Real client request shapes: Claude Code 2.1 (minus its identity lines,
    # which Antigravity rejects) and Codex CLI 0.164.
    LOOKUP_SCHEMA = {
        "$schema": "http://json-schema.org/draft-07/schema#",
        "type": "object",
        "properties": {
            "path": {"type": "string", "examples": ["/etc/hosts"]},
            "mode": {"type": ["string", "null"], "enum": ["read", "stat", None]},
            "depth": {"type": "integer", "exclusiveMinimum": 0},
            "target": {"$ref": "#/$defs/Target"},
            "filter": {"anyOf": [{"type": "string"}, {"type": "null"}]},
        },
        "required": ["path"],
        "additionalProperties": False,
        "$defs": {
            "Target": {"type": "object", "properties": {"host": {"type": "string"}}}
        },
    }

    def claude_code_shape(self, model):
        payload = {
            "model": model,
            "max_tokens": 16000,
            "system": [
                {
                    "type": "text",
                    "text": "You are a coding agent. Use tools when asked.",
                },
                {
                    "type": "text",
                    "text": "Be brief.",
                    "cache_control": {"type": "ephemeral"},
                },
            ],
            "messages": [
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "text",
                            "text": "Call mcp__fs.local__lookup with path /etc/hosts and mode read.",
                            "cache_control": {"type": "ephemeral"},
                        }
                    ],
                },
                {
                    "role": "system",
                    "content": [{"type": "text", "text": "Environment: linux."}],
                },
            ],
            "tools": [
                {
                    "name": "mcp__fs.local__lookup",
                    "description": "Look up a file",
                    "input_schema": self.LOOKUP_SCHEMA,
                    "cache_control": {"type": "ephemeral"},
                },
                {
                    "name": "Deferred",
                    "description": "not loaded",
                    "input_schema": {"type": "object"},
                    "defer_loading": True,
                },
            ],
            "tool_choice": {"type": "auto", "disable_parallel_tool_use": True},
            "thinking": {"type": "adaptive", "display": "omitted"},
            "output_config": {"effort": "medium"},
            "context_management": {
                "edits": [{"type": "clear_thinking_20251015", "keep": "all"}]
            },
            "metadata": {"user_id": "live-validation"},
        }
        r = self.raw("/v1/messages", payload)
        calls = [b for b in r["content"] if b["type"] == "tool_use"]
        assert calls and calls[0]["name"] == "mcp__fs.local__lookup", r["content"]
        assert calls[0]["input"].get("path") == "/etc/hosts", calls[0]["input"]
        assert not any(
            b["type"] == "thinking" and b["thinking"] for b in r["content"]
        ), "hidden thinking exposed"
        payload["messages"] += [
            {"role": "assistant", "content": r["content"]},
            {
                "role": "user",
                "content": [
                    {
                        "type": "tool_result",
                        "tool_use_id": calls[0]["id"],
                        "content": "127.0.0.1 localhost",
                        "cache_control": {"type": "ephemeral"},
                    }
                ],
            },
        ]
        r = self.raw("/v1/messages", payload)
        assert r["stop_reason"] == "end_turn", r
        return {"tool_name_restored": True, "schema_features_cleaned": True, "turns": 2}

    def codex_shape(self, model):
        tools = [
            {
                "type": "function",
                "name": "exec_command",
                "description": "Run a shell command",
                "strict": False,
                "parameters": {
                    "type": "object",
                    "properties": {"cmd": {"type": "string"}},
                    "required": ["cmd"],
                    "additionalProperties": False,
                },
            },
            {
                "type": "namespace",
                "name": "multi_agent_v1",
                "description": "Agents",
                "tools": [
                    {
                        "type": "function",
                        "name": "close_agent",
                        "description": "Close an agent",
                        "strict": False,
                        "parameters": {
                            "type": "object",
                            "properties": {"target": {"type": "string"}},
                            "required": ["target"],
                            "additionalProperties": False,
                        },
                    }
                ],
            },
            {
                "type": "custom",
                "name": "apply_patch",
                "description": "Apply a patch",
                "format": {
                    "type": "grammar",
                    "syntax": "lark",
                    "definition": "start: /.+/",
                },
            },
            {"type": "web_search", "external_web_access": False},
        ]
        history = [
            {
                "type": "message",
                "role": "developer",
                "content": [{"type": "input_text", "text": "You are a coding agent."}],
            },
            {
                "type": "message",
                "role": "user",
                "content": [
                    {"type": "input_text", "text": "Call close_agent with target A1."}
                ],
            },
        ]
        payload = {
            "model": model,
            "stream": False,
            "store": False,
            "tools": tools,
            "tool_choice": "auto",
            "parallel_tool_calls": False,
            "include": ["reasoning.encrypted_content"],
            "reasoning": {"effort": "xhigh", "summary": "auto"},
            "prompt_cache_key": "live",
            "client_metadata": {"session_id": "live"},
            "input": history,
        }
        r = self.raw("/v1/responses", payload)
        calls = [i for i in r["output"] if i["type"] == "function_call"]
        assert (
            calls
            and calls[0]["name"] == "close_agent"
            and calls[0].get("namespace") == "multi_agent_v1"
        ), r["output"]
        payload["input"] = (
            history
            + r["output"]
            + [
                {
                    "type": "function_call_output",
                    "call_id": calls[0]["call_id"],
                    "output": "closed",
                }
            ]
        )
        r = self.raw("/v1/responses", payload)
        assert r["status"] == "completed" and any(
            i["type"] == "message" for i in r["output"]
        ), r
        return {"namespace_restored": True, "turns": 2}

    def tool_turn(self, model):
        tools = [
            {
                "name": "add",
                "description": "Add two integers",
                "input_schema": {
                    "type": "object",
                    "properties": {"a": {"type": "integer"}, "b": {"type": "integer"}},
                    "required": ["a", "b"],
                },
            }
        ]
        prompt = "Use the add tool for 17+25, then reply with ANSWER=<n>."
        first = self.a.messages.create(
            model=model,
            max_tokens=8000,
            tools=tools,
            messages=[{"role": "user", "content": prompt}],
            thinking={"type": "enabled", "budget_tokens": 2048},
        )
        use = next(b for b in first.content if b.type == "tool_use")
        return tools, [
            {"role": "user", "content": prompt},
            {
                "role": "assistant",
                "content": [b.model_dump(exclude_none=True) for b in first.content],
            },
            {
                "role": "user",
                "content": [
                    {"type": "tool_result", "tool_use_id": use.id, "content": "42"}
                ],
            },
        ]

    def lost_state(self):
        turns = {}
        for model in ["gemini-3-flash", "gemini-3.8-flash-medium"]:
            turns[model] = self.tool_turn(model)
        # Losing state.db drops every replay record and the sealing key.
        self.process.terminate()
        self.process.wait(timeout=10)
        (self.root / "data" / "state.db").unlink()
        self.start()
        for model, (tools, messages) in turns.items():
            r = self.a.messages.create(
                model=model,
                max_tokens=8000,
                tools=tools,
                messages=messages,
                thinking={"type": "enabled", "budget_tokens": 2048},
            )
            assert "42" in self.text(r), f"{model}: {self.text(r)}"
        return {"continued_after_state_loss": list(turns)}

    def model_switch(self):
        # Signatures are bound to the model that produced them.
        pairs = [
            ("gemini-3-flash", "gemini-3.8-flash-medium"),
            ("gemini-3.8-flash-medium", "gemini-3-flash"),
        ]
        for first, second in pairs:
            tools, messages = self.tool_turn(first)
            r = self.a.messages.create(
                model=second, max_tokens=8000, tools=tools, messages=messages
            )
            assert "42" in self.text(r), f"{first} -> {second}: {self.text(r)}"
        return {"switched": [f"{a} -> {b}" for a, b in pairs]}

    def gemini_only(self):
        models = [m.id for m in self.o.models.list()]
        assert models and not any("claude" in m or "gpt" in m for m in models), models
        try:
            self.request("responses", "claude-sonnet-4-6", "hi")
        except openai.BadRequestError as error:
            assert "Gemini" in str(error), error
        else:
            raise AssertionError("a non-Gemini model was served")
        return {"models": len(models)}

    def logged(self, text):
        self.log.flush()
        self.log.seek(0)
        return text in self.log.read()

    def turn_end(self, proto, model):
        # Antigravity rejects requests that end with an assistant turn; the
        # service appends a user turn so the model continues the reply.
        prompt = [
            {
                "role": "user",
                "content": "List the colors red, green and blue, comma separated.",
            },
            {"role": "assistant", "content": "red,"},
        ]
        r = self.request(proto, model, prompt)
        text = self.text(r).lower()
        assert "green" in text or "blue" in text, text
        assert self.logged("appended a user turn"), "no continuation turn logged"
        return self.details(r)

    def trailing_reasoning(self):
        # At high effort Gemini returns its thinking as a reasoning item before the answer.
        prompt = "Compute 23*19."
        r = self.o.responses.create(
            model="gemini-3-flash",
            input=prompt,
            reasoning={"effort": "high", "summary": "auto"},
            store=False,
        )
        reasoning = [
            v.model_dump(exclude_none=True)
            for v in r.output
            if v.type == "reasoning" and v.summary
        ]
        assert reasoning, "no reasoning item to replay"
        follow = self.o.responses.create(
            model="gemini-3-flash",
            input=[{"role": "user", "content": prompt}, reasoning[0]],
            store=False,
        )
        assert "437" in follow.output_text, follow.output_text
        assert self.logged("dropped trailing reasoning"), "trailing reasoning kept"
        return self.details(follow)

    COMPACTION_HISTORY = [
        {
            "role": "user",
            "content": "My project codename is BLUEFIN-42. Check the build.",
        },
        {
            "type": "function_call",
            "call_id": "call_build",
            "name": "exec_command",
            "arguments": '{"cmd":"make build"}',
        },
        {
            "type": "function_call_output",
            "call_id": "call_build",
            "output": "build ok in 3.2s",
        },
        {
            "role": "assistant",
            "content": "The build of BLUEFIN-42 passed in 3.2 seconds.",
        },
    ]
    COMPACTION_TOOLS = [
        {
            "type": "function",
            "name": "exec_command",
            "parameters": {
                "type": "object",
                "properties": {"cmd": {"type": "string"}},
                "required": ["cmd"],
            },
        }
    ]

    def compaction(self, model, endpoint):
        payload = {"model": model, "tools": self.COMPACTION_TOOLS, "store": False}
        if endpoint:
            compacted = self.raw(
                "/v1/responses/compact", dict(payload, input=self.COMPACTION_HISTORY)
            )
            assert compacted["object"] == "response.compaction", compacted
        else:
            compacted = self.raw(
                "/v1/responses",
                dict(
                    payload,
                    input=self.COMPACTION_HISTORY + [{"type": "compaction_trigger"}],
                ),
            )
        items = compacted["output"]
        assert [i["type"] for i in items] == ["compaction"], items
        follow = self.o.responses.create(
            model=model,
            tools=self.COMPACTION_TOOLS,
            input=items
            + [
                {
                    "role": "user",
                    "content": "What is my project codename and how long did the build take? Answer without tools.",
                }
            ],
            store=False,
        )
        text = follow.output_text
        assert "BLUEFIN-42" in text and "3.2" in text, text
        return {"usage": compacted["usage"], "answer": text[:160]}

    def user_agent(self):
        self.log.flush()
        self.log.seek(0)
        for line in self.log:
            if "Antigravity version check" in line:
                entry = json.loads(line)
                assert entry["user_agent"].startswith("antigravity/"), entry
                return {
                    "user_agent": entry["user_agent"],
                    "latest": entry.get("latest"),
                }
        raise AssertionError("no version check logged")

    def search_routed(self):
        # gemini-3-flash is not a search model; the search goes to one that is.
        r = self.request(
            "messages",
            "gemini-3-flash",
            "Search the web: what is the latest stable Python release and when was it released? Cite your sources.",
            tools=[{"type": "web_search_20250305", "name": "web_search"}],
        )
        citations = [
            c for b in r.content if b.type == "text" for c in (b.citations or [])
        ]
        assert citations, "no citations after routing search"
        assert self.logged("web search routed"), "search was not routed"
        return dict(self.details(r), citation_count=len(citations))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", type=Path, default=Path("config.json"))
    parser.add_argument("--binary", type=Path, default=Path("bin/agy2api"))
    parser.add_argument("--report", type=Path, default=Path("tests/live-report.json"))
    parser.add_argument("--only", action="append", default=[])
    args = parser.parse_args()
    suite = Live(args.config, args.binary, args.report)
    try:
        cases = []
        models = [m.id for m in suite.o.models.list()]
        for model in models:
            if "image" not in model:
                cases.append(
                    (f"catalog/{model}", lambda m=model: suite.basic("responses", m))
                )
        for proto in ["responses", "messages"]:
            for model in [
                "gemini-3-flash",
                "gemini-3.8-flash-medium",
                "gemini-2.5-flash",
            ]:
                cases += [
                    (
                        f"{proto}/{model}/basic",
                        lambda p=proto, m=model: suite.basic(p, m),
                    ),
                    (
                        f"{proto}/{model}/stream",
                        lambda p=proto, m=model: suite.streaming(p, m),
                    ),
                    (
                        f"{proto}/{model}/tools",
                        lambda p=proto, m=model: suite.tools(p, m),
                    ),
                ]
            for model in ["gemini-3-flash", "gemini-3.8-flash-medium"]:
                cases += [
                    (
                        f"{proto}/{model}/parallel_tools",
                        lambda p=proto, m=model: suite.tools(p, m, parallel=True),
                    ),
                    (
                        f"{proto}/{model}/thinking",
                        lambda p=proto, m=model: suite.thinking(p, m),
                    ),
                    (
                        f"{proto}/{model}/thinking_tools",
                        lambda p=proto, m=model: suite.tools(p, m, thinking=True),
                    ),
                    (
                        f"{proto}/{model}/structured",
                        lambda p=proto, m=model: suite.structured(p, m),
                    ),
                    (
                        f"{proto}/{model}/image_input",
                        lambda p=proto, m=model: suite.media(p, m, "image"),
                    ),
                    (
                        f"{proto}/{model}/pdf_input",
                        lambda p=proto, m=model: suite.media(p, m, "pdf"),
                    ),
                    (
                        f"{proto}/{model}/count",
                        lambda p=proto, m=model: suite.count(p, m),
                    ),
                    (
                        f"{proto}/{model}/limit",
                        lambda p=proto, m=model: suite.limits(p, m),
                    ),
                ]
                cases.append(
                    (
                        f"{proto}/{model}/tool_image",
                        lambda p=proto, m=model: suite.tool_image(p, m),
                    )
                )
                if proto == "messages":
                    cases.append(
                        (
                            f"{proto}/{model}/stop_sequence",
                            lambda m=model: suite.stop_sequence(m),
                        )
                    )
            cases.append((f"{proto}/search", lambda p=proto: suite.search(p)))
            cases += [
                (f"{proto}/system_priority", lambda p=proto: suite.system(p)),
                (f"{proto}/long_context", lambda p=proto: suite.long_context(p)),
                (
                    f"{proto}/search_stream_unicode",
                    lambda p=proto: suite.search_stream(p),
                ),
                (f"{proto}/schema_tool_fields", lambda p=proto: suite.schema_tool(p)),
            ]
            for model in ["gemini-3.8-flash-high", "gemini-2.5-flash"]:
                cases += [
                    (
                        f"{proto}/{model}/thinking",
                        lambda p=proto, m=model: suite.thinking(p, m),
                    ),
                    (
                        f"{proto}/{model}/thinking_tools",
                        lambda p=proto, m=model: suite.tools(p, m, thinking=True),
                    ),
                ]
        cases += [
            ("media/https_image", suite.remote_image),
            ("media/image_generation", suite.image_generation),
            ("http/cancellation", suite.cancellation),
            ("http/negative", suite.negative),
        ]
        for model in ["gemini-3-flash", "gemini-3.8-flash-medium"]:
            cases += [
                (
                    f"compat/claude_code/{model}",
                    lambda m=model: suite.claude_code_shape(m),
                ),
                (f"compat/codex/{model}", lambda m=model: suite.codex_shape(m)),
            ]
        cases += [
            ("compat/model_switch", suite.model_switch),
            ("catalog/gemini_only", suite.gemini_only),
            ("messages/search_routed", suite.search_routed),
            ("compat/trailing_reasoning", suite.trailing_reasoning),
            ("upstream/user_agent", suite.user_agent),
        ]
        for model in ["gemini-3-flash", "gemini-3.8-flash-medium", "gemini-2.5-flash"]:
            for proto in ["responses", "messages"]:
                cases.append(
                    (
                        f"compat/turn_end/{proto}/{model}",
                        lambda p=proto, m=model: suite.turn_end(p, m),
                    )
                )
        for model in ["gemini-3-flash", "gemini-3.8-flash-medium"]:
            cases += [
                (
                    f"compaction/trigger/{model}",
                    lambda m=model: suite.compaction(m, endpoint=False),
                ),
                (
                    f"compaction/endpoint/{model}",
                    lambda m=model: suite.compaction(m, endpoint=True),
                ),
            ]
        cases += [
            ("media/audio_file", lambda: suite.av_file("audio")),
            ("media/video_file", lambda: suite.av_file("video")),
            ("state/reasoning_tamper", suite.reasoning_tamper),
        ]
        cases.append(("media/image_edit_roundtrip", suite.image_edit_roundtrip))
        selected = [
            (name, fn)
            for name, fn in cases
            if not args.only or any(pattern in name for pattern in args.only)
        ]
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            futures = [pool.submit(suite.run_case, name, fn) for name, fn in selected]
            for future in futures:
                future.result()
        for name, fn in [
            ("state/restart", suite.persistence),
            ("state/lost_state", suite.lost_state),
            ("auth/refresh", suite.refresh),
            ("http/concurrency", suite.concurrency),
        ]:
            if not args.only or any(pattern in name for pattern in args.only):
                suite.run_case(name, fn)
        print(
            json.dumps(
                {
                    "passed": sum(r["status"] == "pass" for r in suite.results),
                    "partial": sum(r["status"] == "partial" for r in suite.results),
                    "blocked": sum(r["status"] == "blocked" for r in suite.results),
                    "failed": sum(r["status"] == "fail" for r in suite.results),
                    "report": str(args.report),
                }
            ),
            flush=True,
        )
    finally:
        suite.close()


if __name__ == "__main__":
    main()
