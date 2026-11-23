"""Run against a local guacd and the demo bridge. Uses the Python standard library."""
import argparse
import os
import socket

parser = argparse.ArgumentParser()
parser.add_argument("--guacd-host", default="127.0.0.1")
parser.add_argument("--guacd-port", default=4822, type=int)
parser.add_argument("--vnc-port", default=5901, type=int)
parser.add_argument("--size", default="640x400",
                    help="expected display size WxH, or 'any' for real hardware")
args = parser.parse_args()
sock = socket.create_connection((args.guacd_host, args.guacd_port), 10)
sock.settimeout(10)
stream = sock.makefile("rb")

def send(*fields):
    sock.sendall((",".join(f"{len(s)}.{s}" for s in fields) + ";").encode())

def receive():
    fields = []
    while True:
        length = bytearray()
        while True:
            char = stream.read(1)
            if char == b".":
                break
            if not char.isdigit() or len(length) > 8:
                raise ValueError("invalid guacd response")
            length.extend(char)
        fields.append(stream.read(int(length)).decode())
        delimiter = stream.read(1)
        if delimiter == b";":
            return fields
        if delimiter != b",":
            raise ValueError("invalid instruction delimiter")

send("select", "vnc")
op, *params = receive()
assert op == "args", op
values = dict(hostname="127.0.0.1", port=str(args.vnc_port),
              password=os.getenv("VNC_PASSWORD", ""))
send("size", "640", "400", "96")
send("audio")
send("video")
send("image", "image/png")
send("connect", *(p if p.startswith("VERSION_") else values.get(p, "") for p in params))
saw_size = False
saw_image = False
for _ in range(200):
    op, *fields = receive()
    if op == "error":
        raise RuntimeError(fields)
    if op == "size" and fields[0] == "0" and (
            args.size == "any" or "x".join(fields[1:3]) == args.size):
        saw_size = fields[1] + "x" + fields[2]
    if op in ("img", "png", "jpeg"):
        saw_image = True
    if op == "sync":
        send("sync", fields[0])
    if saw_size and saw_image and op in ("end", "sync"):
        print(f"guacd received the {saw_size} display and encoded image")
        break
else:
    raise RuntimeError("missing framebuffer")
send("disconnect")
sock.close()
