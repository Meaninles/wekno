#!/usr/bin/env python3
"""Read-only Redis/Asynq key inventory without reading task payload values."""

from __future__ import annotations

import argparse
import csv
import os
import socket
import ssl
import sys


class RedisConnection:
    def __init__(self, host: str, port: int, use_tls: bool) -> None:
        raw = socket.create_connection((host, port), timeout=10)
        if use_tls:
            context = ssl.create_default_context()
            if os.getenv("REDIS_TLS_INSECURE_SKIP_VERIFY", "").lower() == "true":
                context.check_hostname = False
                context.verify_mode = ssl.CERT_NONE
            server_name = os.getenv("REDIS_TLS_SERVER_NAME") or host
            raw = context.wrap_socket(raw, server_hostname=server_name)
        self.socket = raw
        self.reader = raw.makefile("rb")

    def close(self) -> None:
        self.reader.close()
        self.socket.close()

    def command(self, *parts: str | bytes | int):
        self.socket.sendall(self._encode(parts))
        return self._read()

    def pipeline(self, commands: list[tuple[str | bytes | int, ...]]) -> list:
        self.socket.sendall(b"".join(self._encode(parts) for parts in commands))
        return [self._read() for _ in commands]

    @staticmethod
    def _encode(parts) -> bytes:
        encoded = []
        for part in parts:
            if isinstance(part, bytes):
                encoded.append(part)
            else:
                encoded.append(str(part).encode())
        payload = [f"*{len(encoded)}\r\n".encode()]
        for part in encoded:
            payload.extend((f"${len(part)}\r\n".encode(), part, b"\r\n"))
        return b"".join(payload)

    def _read(self):
        prefix = self.reader.read(1)
        if not prefix:
            raise RuntimeError("Redis closed the connection")
        line = self.reader.readline()
        if not line.endswith(b"\r\n"):
            raise RuntimeError("invalid Redis response")
        value = line[:-2]
        if prefix == b"+":
            return value
        if prefix == b"-":
            raise RuntimeError("Redis error: " + value.decode(errors="replace"))
        if prefix == b":":
            return int(value)
        if prefix == b"$":
            size = int(value)
            if size == -1:
                return None
            body = self.reader.read(size)
            if self.reader.read(2) != b"\r\n":
                raise RuntimeError("invalid Redis bulk response")
            return body
        if prefix == b"*":
            size = int(value)
            if size == -1:
                return None
            return [self._read() for _ in range(size)]
        raise RuntimeError(f"unsupported Redis response prefix {prefix!r}")


def split_address(address: str) -> tuple[str, int]:
    address = address.strip()
    if address.startswith("["):
        host, _, suffix = address[1:].partition("]")
        return host, int(suffix.lstrip(":") or "6379")
    host, separator, port = address.rpartition(":")
    if not separator:
        return address, 6379
    return host, int(port)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--address", default=os.getenv("REDIS_ADDR", ""))
    parser.add_argument("--database", type=int, default=int(os.getenv("REDIS_DB", "0") or 0))
    args = parser.parse_args()
    if not args.address:
        parser.error("--address or REDIS_ADDR is required")
    host, port = split_address(args.address)
    connection = RedisConnection(
        host,
        port,
        os.getenv("REDIS_TLS_ENABLED", "").lower() == "true",
    )
    try:
        username = os.getenv("REDIS_USERNAME", "")
        password = os.getenv("REDIS_PASSWORD", "")
        if password:
            if username:
                connection.command("AUTH", username, password)
            else:
                connection.command("AUTH", password)
        connection.command("SELECT", args.database)

        prefix = os.getenv("REDIS_PREFIX", "")
        patterns = ["asynq:*"]
        if prefix:
            patterns.append(prefix + "asynq:*")
        keys: set[bytes] = set()
        for pattern in patterns:
            cursor = b"0"
            while True:
                result = connection.command("SCAN", cursor, "MATCH", pattern, "COUNT", 1000)
                cursor = result[0]
                keys.update(result[1])
                if cursor == b"0":
                    break

        writer = csv.writer(sys.stdout, lineterminator="\n")
        writer.writerow(("key", "redis_type", "member_count", "ttl_ms"))
        sorted_keys = sorted(keys)
        for offset in range(0, len(sorted_keys), 1000):
            batch = sorted_keys[offset : offset + 1000]
            redis_types = [
                value.decode()
                for value in connection.pipeline([("TYPE", key) for key in batch])
            ]
            size_commands = []
            for key, redis_type in zip(batch, redis_types):
                command = {
                    "string": "STRLEN",
                    "list": "LLEN",
                    "set": "SCARD",
                    "zset": "ZCARD",
                    "hash": "HLEN",
                    "stream": "XLEN",
                }.get(redis_type)
                size_commands.append((command, key) if command else ("EXISTS", key))
            sizes = connection.pipeline(size_commands)
            ttls = connection.pipeline([("PTTL", key) for key in batch])
            for key, redis_type, size, ttl in zip(batch, redis_types, sizes, ttls):
                writer.writerow(
                    (
                        key.decode(errors="backslashreplace"),
                        redis_type,
                        int(size) if redis_type != "none" else -1,
                        int(ttl),
                    )
                )
    finally:
        connection.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
