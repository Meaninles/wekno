from __future__ import annotations

import argparse
import base64
import os
import subprocess
import sys
from pathlib import Path

from cryptography.hazmat.primitives.ciphers.aead import AESGCM


ENC_PREFIX = "enc:v1:"


def load_dotenv(path: Path) -> dict[str, str]:
    values: dict[str, str] = {}
    for raw_line in path.read_text(encoding="utf-8-sig").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        name, value = line.split("=", 1)
        name = name.strip()
        value = value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in {'"', "'"}:
            value = value[1:-1]
        values[name] = value
    return values


def load_encrypted_api_key(container: str, tenant_id: int) -> str:
    completed = subprocess.run(
        [
            "docker",
            "exec",
            container,
            "psql",
            "-U",
            "postgres",
            "-d",
            "WeKnora",
            "-X",
            "-qAt",
            "-v",
            "ON_ERROR_STOP=1",
            "-c",
            f"SELECT api_key FROM tenants WHERE id={tenant_id:d}",
        ],
        check=False,
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
    )
    if completed.returncode != 0:
        raise RuntimeError(
            "unable to read the local tenant API key: "
            + completed.stderr.strip()[:1000]
        )
    value = completed.stdout.strip()
    if not value:
        raise RuntimeError(f"local tenant {tenant_id} has no API key")
    return value


def decrypt_stored_secret(encrypted: str, key: str) -> str:
    if not encrypted.startswith(ENC_PREFIX):
        return encrypted
    key_bytes = key.encode("utf-8")
    if len(key_bytes) != 32:
        raise RuntimeError("SYSTEM_AES_KEY must be exactly 32 bytes")
    encoded = encrypted[len(ENC_PREFIX) :]
    encoded += "=" * (-len(encoded) % 4)
    combined = base64.urlsafe_b64decode(encoded)
    if len(combined) < 12 + 16:
        raise RuntimeError("encrypted local tenant API key is too short")
    return AESGCM(key_bytes).decrypt(combined[:12], combined[12:], None).decode("utf-8")


def main() -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Run the real XLSX local API E2E with the existing tenant key "
            "decrypted only in memory."
        )
    )
    parser.add_argument("--env-file", type=Path, default=Path(".env"))
    parser.add_argument("--tenant-id", type=int, default=10000)
    parser.add_argument("--postgres-container", default="WeKnora-postgres-dev")
    args, child_args = parser.parse_known_args()
    values = load_dotenv(args.env_file.resolve())
    api_key = decrypt_stored_secret(
        load_encrypted_api_key(args.postgres_container, args.tenant_id),
        values.get("SYSTEM_AES_KEY", ""),
    )
    os.environ["WEKNORA_E2E_TENANT_API_KEY"] = api_key
    # Keep the credential out of argv, stdout, files and subprocess command
    # lines. The imported runner consumes it directly from this process env.
    import run_production_xlsx_local_e2e

    sys.argv = ["run_production_xlsx_local_e2e.py", *child_args]
    return run_production_xlsx_local_e2e.main()


if __name__ == "__main__":
    raise SystemExit(main())
