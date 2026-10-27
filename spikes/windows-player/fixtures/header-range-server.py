#!/usr/bin/env python3
"""Loopback-only media fixture with exact Header and byte-range checks."""

from __future__ import annotations

import argparse
import json
import re
import secrets
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlsplit


RANGE_PATTERN = re.compile(r"^bytes=(\d*)-(\d*)$")


def parse_single_range(value: str, size: int) -> tuple[int, int] | None:
    match = RANGE_PATTERN.fullmatch(value.strip())
    if not match:
        return None
    start_text, end_text = match.groups()
    if not start_text and not end_text:
        return None
    if not start_text:
        suffix = int(end_text)
        if suffix <= 0:
            return None
        start = max(size - suffix, 0)
        end = size - 1
    else:
        start = int(start_text)
        end = int(end_text) if end_text else size - 1
    if start >= size or end < start:
        return None
    return start, min(end, size - 1)


class FixtureHandler(BaseHTTPRequestHandler):
    server_version = "DoriyaFixture/1"
    sys_version = ""

    def do_HEAD(self) -> None:
        self._serve(send_body=False)

    def do_GET(self) -> None:
        self._serve(send_body=True)

    def _serve(self, *, send_body: bool) -> None:
        fixture = self.server.fixture  # type: ignore[attr-defined]
        path = urlsplit(self.path).path
        if path != fixture["token_path"]:
            self._plain(HTTPStatus.NOT_FOUND)
            return

        if (
            self.headers.get("Referer") != fixture["referer"]
            or self.headers.get("User-Agent") != fixture["user_agent"]
        ):
            self._plain(HTTPStatus.FORBIDDEN)
            return

        size = fixture["size"]
        range_header = self.headers.get("Range")
        start = 0
        end = size - 1
        status = HTTPStatus.OK
        if range_header is not None:
            parsed = parse_single_range(range_header, size)
            if parsed is None:
                self.send_response(HTTPStatus.REQUESTED_RANGE_NOT_SATISFIABLE)
                self.send_header("Content-Range", f"bytes */{size}")
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            start, end = parsed
            status = HTTPStatus.PARTIAL_CONTENT

        length = end - start + 1
        self.send_response(status)
        self.send_header("Accept-Ranges", "bytes")
        self.send_header("Content-Type", fixture["content_type"])
        self.send_header("Content-Length", str(length))
        if status == HTTPStatus.PARTIAL_CONTENT:
            self.send_header("Content-Range", f"bytes {start}-{end}/{size}")
        self.end_headers()

        if not send_body:
            return
        with fixture["path"].open("rb") as media:
            media.seek(start)
            remaining = length
            while remaining:
                chunk = media.read(min(remaining, 64 * 1024))
                if not chunk:
                    break
                self.wfile.write(chunk)
                remaining -= len(chunk)

    def _plain(self, status: HTTPStatus) -> None:
        body = f"{status.value} {status.phrase}\n".encode("ascii")
        self.send_response(status)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def log_message(self, _format: str, *_args: object) -> None:
        return


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--file", type=Path, required=True)
    parser.add_argument("--port", type=int, default=0)
    parser.add_argument("--referer", default="https://www.bilibili.com/")
    parser.add_argument("--user-agent", default="Doriya-W0-Test/1")
    args = parser.parse_args()

    media_path = args.file.resolve(strict=True)
    if not media_path.is_file():
        parser.error("--file must be a regular file")

    token = secrets.token_urlsafe(24)
    server = ThreadingHTTPServer(("127.0.0.1", args.port), FixtureHandler)
    server.fixture = {  # type: ignore[attr-defined]
        "path": media_path,
        "size": media_path.stat().st_size,
        "token_path": f"/{token}/{media_path.name}",
        "referer": args.referer,
        "user_agent": args.user_agent,
        "content_type": "video/mp4" if media_path.suffix == ".mp4" else "video/x-matroska",
    }

    print(
        json.dumps(
            {
                "url": f"http://127.0.0.1:{server.server_port}{server.fixture['token_path']}",
                "referer": server.fixture["referer"],
                "user_agent": server.fixture["user_agent"],
                "size": server.fixture["size"],
            }
        ),
        flush=True,
    )
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
