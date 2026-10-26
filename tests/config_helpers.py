"""TOML fixtures for the flat application config (Python 3.11+)."""

import json
import tomllib


def read_config(path):
    with path.open("rb") as source:
        return tomllib.load(source)


def write_config(path, values):
    # JSON scalar spelling is also valid TOML for our strings, integers and
    # booleans. This is a fixture writer, not a general TOML serializer.
    lines = []
    for key, value in values.items():
        if type(value) not in (str, int, bool):
            raise TypeError(f"unsupported config fixture type for {key}")
        lines.append(f"{json.dumps(key)} = {json.dumps(value, ensure_ascii=False)}\n")
    path.touch(mode=0o600, exist_ok=True)
    path.chmod(0o600)
    path.write_text("".join(lines), encoding="utf-8")
