"""Small synthetic media inputs shared by SDK and live tests."""

import struct
import zlib


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
