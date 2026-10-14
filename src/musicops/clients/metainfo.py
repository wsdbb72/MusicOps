"""Bounded canonical bencode parsing; v1, BEP 52 v2 and hybrid identities."""
from __future__ import annotations

import hashlib
import re


def bencode(value):
    """Canonical encoder used for synthetic fixtures, not digest round-trips."""
    if isinstance(value, bytes):
        return str(len(value)).encode() + b":" + value
    if isinstance(value, str):
        return bencode(value.encode())
    if type(value) is int:
        return b"i" + str(value).encode() + b"e"
    if isinstance(value, list):
        return b"l" + b"".join(bencode(x) for x in value) + b"e"
    if isinstance(value, dict):
        items = sorted((k.encode() if isinstance(k, str) else k, v) for k, v in value.items())
        return b"d" + b"".join(bencode(k) + bencode(v) for k, v in items) + b"e"
    raise ValueError("unsupported bencode value")


class _Decoder:
    def __init__(self, data):
        self.data, self.pos, self.info_span, self.nodes = data, 0, None, 0

    def parse(self, depth=0):
        self.nodes += 1
        if depth > 64 or self.nodes > 200000 or self.pos >= len(self.data):
            raise ValueError("metainfo limit or truncation")
        start = self.pos
        token = self.data[self.pos:self.pos + 1]
        if token == b"i":
            end = self.data.find(b"e", self.pos + 1)
            text = self.data[self.pos + 1:end]
            if end < 0 or not re.fullmatch(rb"0|-?[1-9][0-9]{0,19}", text):
                raise ValueError("noncanonical integer")
            self.pos = end + 1
            return int(text)
        if token in (b"d", b"l"):
            self.pos += 1
            values = {} if token == b"d" else []
            previous = None
            while self.pos < len(self.data) and self.data[self.pos:self.pos + 1] != b"e":
                key_or_item = self.parse(depth + 1)
                if token == b"l":
                    values.append(key_or_item)
                    continue
                key = key_or_item
                if not isinstance(key, bytes) or (previous is not None and key <= previous):
                    raise ValueError("duplicate or unordered dictionary key")
                previous = key
                value_start = self.pos
                values[key] = self.parse(depth + 1)
                if depth == 0 and key == b"info":
                    self.info_span = (value_start, self.pos)
            if self.pos >= len(self.data):
                raise ValueError("unterminated container")
            self.pos += 1
            return values
        colon = self.data.find(b":", start, start + 12)
        text = self.data[start:colon]
        if colon < 0 or not re.fullmatch(rb"0|[1-9][0-9]*", text):
            raise ValueError("bad string length")
        length = int(text)
        self.pos = colon + 1 + length
        if self.pos > len(self.data):
            raise ValueError("truncated string")
        return self.data[colon + 1:self.pos]


def _component(raw):
    if not isinstance(raw, bytes):
        raise ValueError("path component must be bytes")
    try:
        name = raw.decode("utf-8")
    except UnicodeError as exc:
        raise ValueError("path UTF-8 invalid; explicit rename required") from exc
    if not name or name in (".", "..") or name.endswith((".", " ")) or re.search(r'[<>:"/\\|?*\x00-\x1f]', name):
        raise ValueError("unsafe Windows path")
    if name.split(".")[0].upper() in {"CON", "PRN", "AUX", "NUL", *[f"COM{x}" for x in range(1, 10)], *[f"LPT{x}" for x in range(1, 10)]}:
        raise ValueError("reserved Windows path")
    return name


def _length(value):
    if type(value) is not int or value < 0:
        raise ValueError("invalid file length")
    return value


def _file(path, raw):
    attrs=raw.get(b"attr",b"")
    if not isinstance(attrs,bytes):
        raise ValueError("invalid file attributes")
    if b"l" in attrs or b"symlink path" in raw:
        raise ValueError("symlink torrent blocked")
    return {"path": "/".join(path), "length": _length(raw.get(b"length")),
            "padding": b"p" in attrs}


def _v1_files(info, name):
    if b"files" in info:
        if b"length" in info or not isinstance(info[b"files"], list) or not info[b"files"]:
            raise ValueError("invalid v1 file list")
        files = []
        for item in info[b"files"]:
            if not isinstance(item, dict) or not isinstance(item.get(b"path"), list) or not item[b"path"]:
                raise ValueError("invalid v1 path")
            files.append(_file([name] + [_component(x) for x in item[b"path"]], item))
        return files
    return [_file([name], info)]


def _v2_files(tree, piece_length, layers):
    if not isinstance(tree, dict) or not tree or b"" in tree:
        raise ValueError("invalid v2 file tree")
    files, referenced = [], set()

    def walk(node, path):
        if not isinstance(node, dict):
            raise ValueError("file tree node")
        if b"" in node:
            if len(node) != 1 or not path or not isinstance(node[b""], dict):
                raise ValueError("file/directory collision")
            properties = node[b""]
            item = _file(path, properties)
            root = properties.get(b"pieces root")
            if item["length"]:
                if not isinstance(root, bytes) or len(root) != 32:
                    raise ValueError("missing v2 pieces root")
                item["pieces_root"] = root.hex()
                if item["length"] > piece_length:
                    layer = layers.get(root)
                    count = (item["length"] + piece_length - 1) // piece_length
                    if not isinstance(layer, bytes) or len(layer) != count * 32:
                        raise ValueError("missing/invalid piece layer")
                    hashes = [layer[x:x + 32] for x in range(0, len(layer), 32)]
                    padding = bytes(32)
                    for _ in range((piece_length // 16384).bit_length() - 1):
                        padding = hashlib.sha256(padding + padding).digest()
                    hashes += [padding] * ((1 << (len(hashes) - 1).bit_length()) - len(hashes))
                    while len(hashes) > 1:
                        hashes = [hashlib.sha256(hashes[x] + hashes[x + 1]).digest() for x in range(0, len(hashes), 2)]
                    if hashes[0] != root:
                        raise ValueError("piece layer root mismatch")
                    referenced.add(root)
            files.append(item)
            if len(files) > 20000:
                raise ValueError("too many files")
            return
        for component, child in node.items():
            walk(child, path + [_component(component)])

    walk(tree, [])
    if set(layers) != referenced:
        raise ValueError("unexpected piece layers")
    return files


def parse_metainfo(data: bytes) -> dict:
    if not isinstance(data, bytes) or not data or len(data) > 8 * 1024 * 1024:
        raise ValueError("metainfo size invalid")
    decoder = _Decoder(data)
    root = decoder.parse()
    if decoder.pos != len(data) or not isinstance(root, dict) or not isinstance(root.get(b"info"), dict) or decoder.info_span is None:
        raise ValueError("invalid metainfo root")
    info = root[b"info"]
    meta_version = info.get(b"meta version")
    if meta_version is not None and (type(meta_version) is not int or meta_version != 2):
        raise ValueError("unsupported metainfo version")
    name = _component(info.get(b"name"))
    piece_length = info.get(b"piece length")
    if type(piece_length) is not int or piece_length < 16384 or piece_length & (piece_length - 1):
        raise ValueError("invalid piece length")
    v1 = b"pieces" in info
    v2 = meta_version == 2
    if not (v1 or v2):
        raise ValueError("no supported metainfo identity")
    v1_files = _v1_files(info, name) if v1 else []
    if v1:
        pieces = info[b"pieces"]
        total = sum(x["length"] for x in v1_files)
        count = (total + piece_length - 1) // piece_length
        if not isinstance(pieces, bytes) or len(pieces) != count * 20:
            raise ValueError("v1 piece count mismatch")
    layers = root.get(b"piece layers")
    if v2 and not isinstance(layers, dict):
        raise ValueError("v2 missing piece layers")
    v2_files = _v2_files(info[b"file tree"], piece_length, layers) if v2 and b"file tree" in info else []
    if v2 and not v2_files:
        raise ValueError("v2 empty file tree")
    if v1 and v2:
        # BEP52 hybrid v1 paths include an advisory root name. v2 paths may be
        # rooted or rootless, so accept only one globally consistent mapping.
        nonpadding = [(x["path"], x["length"]) for x in v1_files if not x["padding"]]
        flat = [(x["path"], x["length"]) for x in v2_files if not x["padding"]]
        rooted = [(name + "/" + p, length) for p, length in flat]
        if nonpadding not in (flat, rooted):
            raise ValueError("hybrid file manifests disagree")
    files = v1_files if v1 else v2_files
    paths = [x["path"].casefold() for x in files]
    if len(paths) != len(set(paths)) or len(files) > 20000:
        raise ValueError("Windows path collision or too many files")
    path_set=set(paths)
    if any('/'.join(path.split('/')[:index]) in path_set for path in paths for index in range(1,len(path.split('/')))):
        raise ValueError("file/directory path collision")
    info_bytes = data[slice(*decoder.info_span)]
    return {"schema_version": 1, "name": name, "type": "hybrid" if v1 and v2 else "v2" if v2 else "v1",
            "infohash_v1": hashlib.sha1(info_bytes).hexdigest() if v1 else None,
            "infohash_v2": hashlib.sha256(info_bytes).hexdigest() if v2 else None,
            "file_list": files, "piece_length": piece_length, "total_size": sum(x["length"] for x in files),
            "metainfo_sha256": hashlib.sha256(data).hexdigest(), "valid": True}
