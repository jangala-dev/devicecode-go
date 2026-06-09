#!/usr/bin/env python3
"""Direct fabric-jsonl/1 UART smoke test for the MCU updater/main transfer path.

This is a host-side diagnostic helper. It speaks the CM5 side of the MCU Fabric
link directly over a serial TTY, without starting the Lua services. It is meant
for the MCU build tagged `fabric_uart_hwtest`, where updater/main staging is a
safe digest/count sink rather than the production A/B flash writer.

Example:

    python3 tools/fabric_uart_xfer_smoke.py /dev/cu.usbserial-110 --size 1024

The script performs:

    hello -> hello_ack
    prepare-update RPC
    xfer_begin / xfer_chunk* / xfer_commit to updater/main
    waits for xfer_done and, where visible, state/self/updater=staged

It deliberately does not require a successful commit-update. In the default
hardware-test build the applier is still refusing, so commit-update should be
left to a later gate.
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import select
import sys
import termios
import time
import tty
from dataclasses import dataclass
from typing import Any, Dict, Iterable, List, Optional

PROTO = "fabric-jsonl/1"
DEFAULT_NODE = "bigbox-cm5"
DEFAULT_PEER = "mcu"
DEFAULT_TARGET = "updater/main"
DEFAULT_EXPECTED_IMAGE = "hwtest-image"
DEFAULT_BAUD = 115200

# Reflected polynomial constants for xxHash32, seed 0.
P1 = 0x9E3779B1
P2 = 0x85EBCA77
P3 = 0xC2B2AE3D
P4 = 0x27D4EB2F
P5 = 0x165667B1


def _u32(v: int) -> int:
    return v & 0xFFFFFFFF


def _rotl32(x: int, r: int) -> int:
    return _u32((x << r) | (x >> (32 - r)))


def _round(acc: int, lane: int) -> int:
    acc = _u32(acc + lane * P2)
    acc = _rotl32(acc, 13)
    acc = _u32(acc * P1)
    return acc


def _read32le(data: bytes, off: int) -> int:
    return data[off] | (data[off + 1] << 8) | (data[off + 2] << 16) | (data[off + 3] << 24)


def xxhash32(data: bytes, seed: int = 0) -> int:
    n = len(data)
    p = 0
    if n >= 16:
        v1 = _u32(seed + P1 + P2)
        v2 = _u32(seed + P2)
        v3 = _u32(seed)
        v4 = _u32(seed - P1)
        limit = n - 16
        while p <= limit:
            v1 = _round(v1, _read32le(data, p)); p += 4
            v2 = _round(v2, _read32le(data, p)); p += 4
            v3 = _round(v3, _read32le(data, p)); p += 4
            v4 = _round(v4, _read32le(data, p)); p += 4
        h = _u32(_rotl32(v1, 1) + _rotl32(v2, 7) + _rotl32(v3, 12) + _rotl32(v4, 18))
    else:
        h = _u32(seed + P5)
    h = _u32(h + n)
    while p + 4 <= n:
        h = _u32(h + _read32le(data, p) * P3)
        h = _u32(_rotl32(h, 17) * P4)
        p += 4
    while p < n:
        h = _u32(h + data[p] * P5)
        h = _u32(_rotl32(h, 11) * P1)
        p += 1
    h ^= h >> 15
    h = _u32(h * P2)
    h ^= h >> 13
    h = _u32(h * P3)
    h ^= h >> 16
    return _u32(h)


def digest_hex(data: bytes) -> str:
    return f"{xxhash32(data):08x}"


def b64url_unpadded(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).decode("ascii").rstrip("=")


@dataclass
class SerialRawConfig:
    old_attrs: List[Any]


class FabricTTY:
    def __init__(self, path: str, baud: int, verbose: bool = False) -> None:
        self.path = path
        self.verbose = verbose
        self.fd = os.open(path, os.O_RDWR | os.O_NOCTTY | os.O_NONBLOCK)
        self._config = self._configure(baud)
        self._rx = bytearray()

    def _configure(self, baud: int) -> SerialRawConfig:
        old = termios.tcgetattr(self.fd)
        attrs = termios.tcgetattr(self.fd)
        tty.setraw(self.fd, termios.TCSANOW)
        attrs = termios.tcgetattr(self.fd)
        speed = getattr(termios, f"B{baud}", None)
        if speed is None:
            raise RuntimeError(f"unsupported baud {baud} on this platform")
        attrs[4] = speed
        attrs[5] = speed
        attrs[2] |= termios.CLOCAL | termios.CREAD
        if hasattr(termios, "CRTSCTS"):
            attrs[2] &= ~termios.CRTSCTS
        attrs[2] &= ~termios.CSTOPB
        attrs[2] &= ~termios.PARENB
        attrs[2] &= ~termios.CSIZE
        attrs[2] |= termios.CS8
        attrs[6][termios.VMIN] = 0
        attrs[6][termios.VTIME] = 0
        termios.tcsetattr(self.fd, termios.TCSANOW, attrs)
        termios.tcflush(self.fd, termios.TCIOFLUSH)
        return SerialRawConfig(old_attrs=old)

    def close(self) -> None:
        try:
            termios.tcsetattr(self.fd, termios.TCSANOW, self._config.old_attrs)
        finally:
            os.close(self.fd)

    def write_msg(self, msg: Dict[str, Any]) -> None:
        line = json.dumps(msg, separators=(",", ":")).encode("utf-8") + b"\n"
        if self.verbose:
            print(">", line.decode("utf-8").rstrip())
        off = 0
        while off < len(line):
            try:
                n = os.write(self.fd, line[off:])
            except BlockingIOError:
                select.select([], [self.fd], [], 0.25)
                continue
            if n > 0:
                off += n

    def read_msg(self, timeout_s: float) -> Dict[str, Any]:
        deadline = time.monotonic() + timeout_s
        while True:
            newline = self._rx.find(b"\n")
            if newline >= 0:
                raw = bytes(self._rx[:newline]).strip()
                del self._rx[: newline + 1]
                if not raw:
                    continue
                try:
                    msg = json.loads(raw.decode("utf-8"))
                except json.JSONDecodeError:
                    print("! ignoring malformed line from peer:", raw.decode("utf-8", "replace"), file=sys.stderr)
                    continue
                if self.verbose:
                    print("<", json.dumps(msg, separators=(",", ":")))
                return msg
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("timed out waiting for fabric frame")
            r, _, _ = select.select([self.fd], [], [], min(0.25, remaining))
            if not r:
                continue
            try:
                chunk = os.read(self.fd, 4096)
            except BlockingIOError:
                continue
            if not chunk:
                continue
            self._rx.extend(chunk)


def wait_for(ttydev: FabricTTY, want_type: str, timeout_s: float, *, want_id: Optional[str] = None) -> Dict[str, Any]:
    deadline = time.monotonic() + timeout_s
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError(f"timed out waiting for {want_type}")
        msg = ttydev.read_msg(remaining)
        mtype = msg.get("type")
        if mtype == "ping":
            ttydev.write_msg({"type": "pong", "sid": msg.get("sid", "")})
            continue
        if mtype == "pub":
            topic = "/".join(str(x) for x in msg.get("topic", []))
            payload = msg.get("payload")
            if topic in {"state/self/software", "state/self/updater", "state/self/health"}:
                print(f"pub {topic}: {json.dumps(payload, separators=(',', ':'))}")
            if want_type == "pub" and (want_id is None or topic == want_id):
                return msg
            continue
        if mtype == want_type and (want_id is None or msg.get("id") == want_id or msg.get("xfer_id") == want_id):
            return msg
        if mtype == "xfer_abort":
            raise RuntimeError(f"peer aborted transfer {msg.get('xfer_id')}: {msg.get('err', '')}")
        if mtype == "reply" and want_type == "reply" and want_id is not None and msg.get("id") != want_id:
            continue
        # Other protocol frames are expected during bring-up and retained export.


def payload_bytes(size: int) -> bytes:
    # Deterministic content with enough variation to exercise chunk digests.
    return bytes(((i * 37 + 11) & 0xFF) for i in range(size))


def transfer(ttydev: FabricTTY, xfer_id: str, target: str, payload: bytes, chunk_size: int, timeout_s: float) -> None:
    whole = digest_hex(payload)
    ttydev.write_msg({
        "type": "xfer_begin",
        "xfer_id": xfer_id,
        "target": target,
        "size": len(payload),
        "digest_alg": "xxhash32",
        "digest": whole,
        "meta": {"source": "tools/fabric_uart_xfer_smoke.py"},
    })
    wait_for(ttydev, "xfer_ready", timeout_s, want_id=xfer_id)
    off = 0
    while off < len(payload):
        part = payload[off : off + chunk_size]
        ttydev.write_msg({
            "type": "xfer_chunk",
            "xfer_id": xfer_id,
            "offset": off,
            "data": b64url_unpadded(part),
            "chunk_digest": digest_hex(part),
        })
        off += len(part)
        need = wait_for(ttydev, "xfer_need", timeout_s, want_id=xfer_id)
        nxt = int(need.get("next", -1))
        if nxt != off:
            raise RuntimeError(f"unexpected xfer_need next={nxt}, want {off}")
        print(f"chunk ack next={off}")
    ttydev.write_msg({
        "type": "xfer_commit",
        "xfer_id": xfer_id,
        "size": len(payload),
        "digest_alg": "xxhash32",
        "digest": whole,
    })
    wait_for(ttydev, "xfer_done", timeout_s, want_id=xfer_id)


def main(argv: Optional[Iterable[str]] = None) -> int:
    p = argparse.ArgumentParser(description="Direct fabric-jsonl/1 UART updater/main transfer smoke test")
    p.add_argument("tty", help="serial device connected to the MCU Fabric UART")
    p.add_argument("--baud", type=int, default=DEFAULT_BAUD)
    p.add_argument("--size", type=int, default=1024, help="test payload bytes")
    p.add_argument("--chunk-size", type=int, default=256, help="raw bytes per xfer_chunk; keep <= 2048")
    p.add_argument("--timeout", type=float, default=10.0)
    p.add_argument("--node", default=DEFAULT_NODE)
    p.add_argument("--peer", default=DEFAULT_PEER)
    p.add_argument("--target", default=DEFAULT_TARGET)
    p.add_argument("--expected-image", default=DEFAULT_EXPECTED_IMAGE)
    p.add_argument("--job-id", default=None)
    p.add_argument("--verbose", action="store_true")
    args = p.parse_args(list(argv) if argv is not None else None)

    if args.chunk_size <= 0 or args.chunk_size > 2048:
        p.error("--chunk-size must be in 1..2048")
    if args.size <= 0:
        p.error("--size must be positive")

    sid = f"cm5-smoke-{int(time.time())}"
    job_id = args.job_id or f"smoke-{int(time.time())}"
    xfer_id = f"xfer-{job_id}"
    payload = payload_bytes(args.size)

    ttydev = FabricTTY(args.tty, args.baud, args.verbose)
    try:
        print(f"hello sid={sid} node={args.node} peer={args.peer}")
        ttydev.write_msg({"type": "hello", "proto": PROTO, "sid": sid, "node": args.node})
        ack = wait_for(ttydev, "hello_ack", args.timeout)
        if ack.get("node") != args.peer or ack.get("proto") != PROTO:
            raise RuntimeError(f"bad hello_ack: {ack}")
        print(f"link up peer_sid={ack.get('sid')}")

        call_id = f"prepare-{job_id}"
        ttydev.write_msg({
            "type": "call",
            "id": call_id,
            "topic": ["cap", "self", "updater", "main", "rpc", "prepare-update"],
            "payload": {
                "job_id": job_id,
                "target": "mcu",
                "expected_image_id": args.expected_image,
                "metadata": {"source": "fabric_uart_xfer_smoke"},
            },
        })
        reply = wait_for(ttydev, "reply", args.timeout, want_id=call_id)
        if not reply.get("ok"):
            raise RuntimeError(f"prepare rejected: {reply}")
        prep = reply.get("payload") or {}
        if prep.get("target") != args.target:
            raise RuntimeError(f"prepare returned target {prep.get('target')!r}, want {args.target!r}")
        max_chunk = int(prep.get("max_chunk_size") or 0)
        if max_chunk and args.chunk_size > max_chunk:
            raise RuntimeError(f"chunk-size {args.chunk_size} exceeds prepare max_chunk_size {max_chunk}")
        print(f"prepare ok target={prep.get('target')} max_chunk_size={max_chunk or 'unknown'}")

        print(f"transfer xfer_id={xfer_id} size={len(payload)} digest={digest_hex(payload)} chunk_size={args.chunk_size}")
        transfer(ttydev, xfer_id, args.target, payload, args.chunk_size, args.timeout)
        print("transfer done")

        # Give retained state export a brief chance to show staged state. The
        # transfer result is authoritative for this smoke test, so this is
        # informational rather than a hard requirement.
        deadline = time.monotonic() + 3.0
        while time.monotonic() < deadline:
            try:
                msg = ttydev.read_msg(max(0.1, deadline - time.monotonic()))
            except TimeoutError:
                break
            if msg.get("type") == "ping":
                ttydev.write_msg({"type": "pong", "sid": msg.get("sid", "")})
                continue
            if msg.get("type") == "pub":
                topic = "/".join(str(x) for x in msg.get("topic", []))
                payload_obj = msg.get("payload")
                if topic in {"state/self/software", "state/self/updater", "state/self/health"}:
                    print(f"pub {topic}: {json.dumps(payload_obj, separators=(',', ':'))}")
        print("smoke ok")
        return 0
    finally:
        ttydev.close()


if __name__ == "__main__":
    raise SystemExit(main())
