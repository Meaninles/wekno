from __future__ import annotations

import importlib.util
import os
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
SHARED_WRAPPER = ROOT / "custom" / "tests" / "document_processing_cluster_e2e" / "run_with_local_tenant_key.py"


def load_secret_helpers():
    spec = importlib.util.spec_from_file_location("weknora_local_key_helpers", SHARED_WRAPPER)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {SHARED_WRAPPER}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def main() -> int:
    helpers = load_secret_helpers()
    values = helpers.load_dotenv(ROOT / ".env")
    encrypted = helpers.load_encrypted_api_key("WeKnora-postgres-dev", 10000)
    os.environ["WEKNORA_E2E_TENANT_API_KEY"] = helpers.decrypt_stored_secret(
        encrypted,
        values.get("SYSTEM_AES_KEY", ""),
    )
    import live_api_e2e

    return live_api_e2e.main()


if __name__ == "__main__":
    raise SystemExit(main())
