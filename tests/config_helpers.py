"""Config and account fixtures for the test scripts (Python 3.11+)."""

import hashlib
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


def account_id(user_id):
    """The account ID agy2api derives from a Google account ID."""
    return hashlib.sha256(b"agy2api account\x00" + user_id.encode()).hexdigest()[:8]


def write_account(data_dir, user_id, api_key, credentials, email, name=""):
    """Write an account as agy2api login does, with a chosen API key."""
    accounts = data_dir / "accounts"
    accounts.mkdir(mode=0o700, parents=True, exist_ok=True)
    directory = accounts / account_id(user_id)
    directory.mkdir(mode=0o700)
    info = {
        "api_key": api_key,
        "user_id": user_id,
        "email": email,
        "name": name,
        "created_at": "2026-01-01T00:00:00Z",
    }
    for file, value in (("credentials.json", credentials), ("account.json", info)):
        path = directory / file
        path.touch(mode=0o600)
        path.write_text(json.dumps(value), encoding="utf-8")
    return directory
