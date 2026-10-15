"""NCM container decryption into a new work copy, with no default media format.

AES uses the pinned cryptography dependency. Metadata is preserved as evidence;
tag editing is a separate format-aware stage and full decoding is still required.
"""
from __future__ import annotations

import base64
import hashlib
import json
import os
from pathlib import Path
import struct
import tempfile

from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes
from cryptography.hazmat.primitives.padding import PKCS7
from musicops.library.paths import reject_reparse_chain
from musicops.domain.errors import PathBlocked

CORE_KEY = b"hzHRAmso5kInbaxW"
META_KEY = bytes.fromhex("2331346c6a6b5f215c5d2630553c2728")


def detect_media(data: bytes) -> str:
    if data.startswith(b"CTENFDAM"):
        return "ncm"
    if data.startswith(b"fLaC"):
        return "flac"
    if len(data) >= 2 and data[0] == 0xff and data[1] & 0xf6 == 0xf0:
        return "aac"
    if data.startswith(b"ID3") or len(data) >= 2 and data[0] == 0xff and data[1] & 0xe0 == 0xe0 and data[1] & 0x06 != 0:
        return "mp3"
    if data.startswith(b"RIFF") and data[8:12] == b"WAVE":
        return "wav"
    if data.startswith(b"OggS"):
        return "ogg"
    if data.startswith(b"MAC "):
        return "ape"
    if data.startswith(b"DSD "):
        return "dsf"
    if len(data) >= 12 and data[4:8] == b"ftyp":
        return "m4a"
    return "unknown"


def _aes_decrypt(data, key):
    if not data or len(data) % 16:
        raise ValueError("invalid AES block length")
    decryptor = Cipher(algorithms.AES(key), modes.ECB()).decryptor()
    plain = decryptor.update(data) + decryptor.finalize()
    unpadder = PKCS7(128).unpadder()
    return unpadder.update(plain) + unpadder.finalize()


def _keybox(key):
    if not key:
        raise ValueError("empty NCM content key")
    box = list(range(256))
    j = 0
    for i in range(256):
        j = (box[i] + j + key[i % len(key)]) & 255
        box[i], box[j] = box[j], box[i]
    return box

def ncm_metadata(path):
    """Read only the bounded encrypted metadata header for import identity."""
    path=Path(path);reject_reparse_chain(path)
    with path.open('rb') as stream:
        def take(size):
            data=stream.read(size)
            if len(data)!=size:raise ValueError('NCM header is truncated')
            return data
        def sized(limit):
            size=struct.unpack('<I',take(4))[0]
            if size>limit:raise ValueError('NCM header is too large')
            return take(size)
        if take(8)!=b'CTENFDAM':raise ValueError('Invalid NCM header')
        take(2);sized(1024*1024);raw=sized(16*1024*1024)
        decoded=bytes(x^0x63 for x in raw);prefix=b"163 key(Don't modify):"
        if not decoded.startswith(prefix):raise ValueError('Invalid NCM metadata')
        plain=_aes_decrypt(base64.b64decode(decoded[len(prefix):],validate=True),META_KEY)
        if not plain.startswith(b'music:'):raise ValueError('Invalid NCM metadata')
        result=json.loads(plain[6:])
        if not isinstance(result,dict):raise ValueError('Invalid NCM metadata')
        return result


def _crypt_payload(data, box, offset=0):
    output = bytearray(len(data))
    for i, value in enumerate(data):
        j = (offset + i + 1) & 255
        output[i] = value ^ box[(box[j] + box[(box[j] + j) & 255]) & 255]
    return bytes(output)


def _hash_file(path):
    digest = hashlib.sha256()
    with Path(path).open("rb") as stream:
        while block := stream.read(1024 * 1024):
            digest.update(block)
    return digest.hexdigest()


def decrypt_ncm(source, destination, tag_writer=None) -> dict:
    try:
        reject_reparse_chain(source)
        reject_reparse_chain(destination)
    except (PathBlocked,OSError):
        return {'schema_version':1,'status':'DECRYPT_FAILED','error_class':'PATH_BLOCKED','success':False,
                'reason':'来源或工作路径含未批准的reparse point'}
    source, destination = Path(source).absolute(), Path(destination).absolute()
    if source == destination or destination.exists():
        return {"schema_version": 1, "status": "CONFLICT", "error_class": "CONFLICT", "success": False,
                "reason": "解密只写新工作副本，不能覆盖源件或已有目标"}
    if not source.is_file():
        return {"schema_version": 1, "status": "DECRYPT_FAILED", "error_class": "PARSE_ERROR", "success": False,
                "reason": "来源不存在或不是普通文件"}
    temporary = None
    before = None
    created_destination = False
    success = False
    stage = "decrypt"
    try:
        before = _hash_file(source)
        with source.open("rb") as stream:
            def take(size):
                value = stream.read(size)
                if len(value) != size:
                    raise ValueError("truncated NCM")
                return value

            def sized(limit):
                size = struct.unpack("<I", take(4))[0]
                if size > limit:
                    raise ValueError("NCM block size exceeds limit")
                return take(size)

            if take(8) != b"CTENFDAM":
                raise ValueError("not an NCM container")
            take(2)
            encrypted_key = bytes(x ^ 0x64 for x in sized(1024 * 1024))
            key = _aes_decrypt(encrypted_key, CORE_KEY)
            if not key.startswith(b"neteasecloudmusic"):
                raise ValueError("invalid NCM key prefix")
            box = _keybox(key[17:])
            metadata_raw = sized(16 * 1024 * 1024)
            metadata = {}
            if metadata_raw:
                decoded = bytes(x ^ 0x63 for x in metadata_raw)
                prefix = b"163 key(Don't modify):"
                if not decoded.startswith(prefix):
                    raise ValueError("invalid metadata prefix")
                metadata_json = _aes_decrypt(base64.b64decode(decoded[len(prefix):], validate=True), META_KEY)
                if not metadata_json.startswith(b"music:"):
                    raise ValueError("invalid metadata payload")
                metadata = json.loads(metadata_json[6:])
                if not isinstance(metadata, dict):
                    raise ValueError("metadata must be a dictionary")
            crc32 = struct.unpack("<I", take(4))[0]
            take(5)
            artwork = sized(32 * 1024 * 1024)
            destination.parent.mkdir(parents=True, exist_ok=True)
            reject_reparse_chain(destination)
            fd, temporary = tempfile.mkstemp(prefix=".ncm-", suffix=".tmp", dir=destination.parent)
            payload_hash, offset, media_format = hashlib.sha256(), 0, None
            with os.fdopen(fd, "wb") as output:
                while block := stream.read(64 * 1024):
                    plain = _crypt_payload(block, box, offset)
                    if media_format is None:
                        media_format = detect_media(plain)
                        if media_format in ("unknown", "ncm"):
                            raise ValueError("decrypted format unknown")
                    output.write(plain)
                    payload_hash.update(plain)
                    offset += len(plain)
                output.flush()
                os.fsync(output.fileno())
            if not offset:
                raise ValueError("empty audio payload")
            declared_format = str(metadata.get("format", "")).lower()
            if declared_format and declared_format != media_format:
                raise ValueError("metadata format conflicts with actual media")
            if destination.suffix and destination.suffix.lower() != "." + media_format:
                raise ValueError("destination extension conflicts with actual media")
            if tag_writer:
                stage = "tags"
                tag_writer(Path(temporary), metadata)
                stage = "decrypt"
            # Use exclusive create rather than replace/rename: no target overwrite
            # even if another process creates the destination during decryption.
            with Path(temporary).open("rb") as prepared, destination.open("xb") as output:
                created_destination = True
                while chunk := prepared.read(64 * 1024):
                    output.write(chunk)
                output.flush()
                os.fsync(output.fileno())
            after = _hash_file(source)
            if before != after:
                raise ValueError("source changed during decrypt")
            result = {"schema_version": 1, "status": "REQUIRES_VALIDATION", "success": False,
                    "path": str(destination), "format": media_format, "source_sha256": before,
                    "output_sha256": _hash_file(destination), "audio_payload_sha256": payload_hash.hexdigest(),
                    "metadata": metadata, "artwork_sha256": hashlib.sha256(artwork).hexdigest() if artwork else None,
                    "artwork_base64": base64.b64encode(artwork).decode() if artwork else None,
                    "container_crc32": crc32, "validation_state": "pending", "source_preserved": True,
                    "tags_prepared": bool(tag_writer)}
            success = True
            return result
    except FileExistsError:
        return {"schema_version": 1, "status": "CONFLICT", "error_class": "CONFLICT", "success": False}
    except PathBlocked:
        return {'schema_version':1,'status':'DECRYPT_FAILED','error_class':'PATH_BLOCKED','success':False,
                'reason':'工作路径在解密过程中发生reparse point变化'}
    except (ValueError, OSError, KeyError, TypeError) as exc:
        return {"schema_version": 1, "status": "TAG_FAILED" if stage == "tags" else "DECRYPT_FAILED", "error_class": "PARSE_ERROR", "success": False,
                "reason": type(exc).__name__, "source_sha256": before}
    except Exception as exc:
        return {"schema_version": 1, "status": "TAG_FAILED", "error_class": "VALIDATION_FAILED", "success": False,
                "reason": type(exc).__name__, "source_sha256": before}
    finally:
        if created_destination and not success:
            # This is a newly created, unpublished work copy, never a source or
            # pre-existing target. Leave any externally replaced file untouched.
            if temporary is not None and destination.exists() and _hash_file(destination) == _hash_file(temporary):
                destination.unlink()
        if temporary is not None:
            Path(temporary).unlink(missing_ok=True)
