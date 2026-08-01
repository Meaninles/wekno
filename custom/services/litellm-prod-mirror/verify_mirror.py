#!/usr/bin/env python3
"""Verify reachable-only selection and the required ``-local`` aliases."""

from __future__ import annotations

import copy
import json
from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parent
SNAPSHOT = ROOT / "config.production.non-glm.yaml"
LOCAL = ROOT / "config.yaml"

REACHABLE_API_BASES = {
    "http://10.0.11.36:30000/v1",
    "http://10.0.11.37:30005/v1",
    "http://10.0.11.36:30004/v1",
    "http://10.0.11.37:30002/v1",
    "http://10.0.11.36:30002/v1",
    "http://10.0.11.36:30003/v1",
}


def load(path: Path) -> dict:
    with path.open("r", encoding="utf-8") as stream:
        value = yaml.safe_load(stream)
    if not isinstance(value, dict):
        raise SystemExit(f"{path} does not contain a YAML object")
    return value


def main() -> None:
    production = load(SNAPSHOT)
    local = load(LOCAL)
    models = production.get("model_list") or []

    glm_entries = [
        item.get("model_name", "")
        for item in models
        if "glm" in str(item.get("model_name", "")).lower()
        or "glm" in str((item.get("litellm_params") or {}).get("model", "")).lower()
    ]
    if glm_entries:
        raise SystemExit(f"GLM entries found in non-GLM snapshot: {glm_entries}")

    expected = copy.deepcopy(production)
    expected["model_list"] = [
        item
        for item in expected.get("model_list") or []
        if (item.get("litellm_params") or {}).get("api_base") in REACHABLE_API_BASES
    ]
    for item in expected.get("model_list") or []:
        item["model_name"] = f"{item['model_name']}-local"

    if local != expected:
        raise SystemExit(
            "local config drifted outside the reachable-entry filter and model_name -local suffix"
        )

    aliases = [item["model_name"] for item in local.get("model_list") or []]
    print(
        json.dumps(
            {
                "status": "ok",
                "deployment_count": len(aliases),
                "unique_model_count": len(set(aliases)),
                "allowed_differences": [
                    "exclude production deployments whose api_base is unreachable locally",
                    "model_list[*].model_name += '-local'",
                ],
                "aliases": sorted(set(aliases)),
            },
            ensure_ascii=False,
            indent=2,
        )
    )


if __name__ == "__main__":
    main()
