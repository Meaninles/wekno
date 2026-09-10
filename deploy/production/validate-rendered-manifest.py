#!/usr/bin/env python3
"""Validate release manifest topology without requiring a YAML dependency."""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path


EXPECTED_NORMAL_REPLICAS = {
    "weknora-app": 3,
    "weknora-parse-worker": 3,
    "weknora-derivative-worker": 2,
    "weknora-wiki-worker": 2,
    "weknora-maintenance": 2,
    "weknora-docreader": 3,
    "weknora-agent-runtime": 2,
    "weknora-frontend": 2,
    "weknora-mobile-web": 2,
}
SUSPENDED_DEPLOYMENTS = {
    "weknora-parse-worker",
    "weknora-derivative-worker",
    "weknora-wiki-worker",
    "weknora-maintenance",
    "weknora-docreader",
    "weknora-agent-runtime",
}
EXPECTED_SCRATCH_PATHS = {
    "/mnt/weknora-data/weknora-v2-scratch/api",
    "/mnt/weknora-data/weknora-v2-scratch/parse",
    "/mnt/weknora-data/weknora-v2-scratch/docreader",
    "/mnt/weknora-data/weknora-v2-scratch/derivative",
    "/mnt/weknora-data/weknora-v2-scratch/wiki",
    "/mnt/weknora-data/weknora-v2-scratch/maintenance",
}
IMAGE_RE = re.compile(r"^[^\s]+@sha256:[0-9a-f]{64}$")
EXPECTED_ROLE_ENV = {
    "weknora-parse-worker": {
        "WEKNORA_ASYNQ_CONCURRENCY": "4",
        "CONCURRENCY_POOL_SIZE": "12",
        "BATCH_EMBED_SIZE": "5",
        "WEKNORA_MULTIMODAL_TASK_CONCURRENCY": "4",
    },
    "weknora-derivative-worker": {
        "WEKNORA_ASYNQ_TASK_CONCURRENCY": "18",
    },
    "weknora-wiki-worker": {
        "WEKNORA_WIKI_MAP_TASK_CONCURRENCY": "6",
    },
}
EXPECTED_TOOL_CALLBACK_URL = (
    "http://app:8080/api/v1/custom/agent-runtime/internal/tools/call"
)


def scalar(document: str, key: str, indent: int = 0) -> str | None:
    match = re.search(rf"(?m)^{' ' * indent}{re.escape(key)}:\s*([^#\n]+?)\s*$", document)
    return match.group(1).strip().strip('"') if match else None


def object_name(document: str) -> str | None:
    metadata = re.search(r"(?ms)^metadata:\s*\n(?P<body>(?:  [^\n]*\n?)*)", document)
    return scalar(metadata.group("body"), "name", 2) if metadata else None


def object_namespace(document: str) -> str | None:
    metadata = re.search(r"(?ms)^metadata:\s*\n(?P<body>(?:  [^\n]*\n?)*)", document)
    return scalar(metadata.group("body"), "namespace", 2) if metadata else None


def parse_documents(path: str) -> list[tuple[str, str, str]]:
    raw = sys.stdin.read() if path == "-" else Path(path).read_text(encoding="utf-8")
    if "REPLACE_" in raw:
        raise ValueError("render contains an unresolved REPLACE_* placeholder")
    result = []
    for document in re.split(r"(?m)^---\s*$", raw):
        kind = scalar(document, "kind")
        if not kind:
            continue
        name = object_name(document)
        if not name:
            raise ValueError(f"{kind} document has no metadata.name")
        result.append((kind, name, document))
    if not result:
        raise ValueError("render contains no Kubernetes objects")
    return result


def validate_images(documents: list[tuple[str, str, str]], errors: list[str]) -> None:
    images = []
    for _, name, document in documents:
        for value in re.findall(r"(?m)^\s+image:\s*\"?([^\s\"]+)\"?\s*$", document):
            images.append((name, value))
    if not images:
        errors.append("render contains no container images")
    for name, value in images:
        if not IMAGE_RE.fullmatch(value):
            errors.append(f"{name} image is not digest-pinned: {value}")


def has_env(document: str, name: str, value: str) -> bool:
    return re.search(
        rf'(?m)^\s*- name:\s*{re.escape(name)}\s*$\n'
        rf'^\s+value:\s*"?{re.escape(value)}"?\s*$',
        document,
    ) is not None


def validate_agent_callback(document: str, errors: list[str]) -> None:
    if not has_env(document, "AGENT_RUNTIME_TOOL_CALLBACK_URL", EXPECTED_TOOL_CALLBACK_URL):
        errors.append("weknora-app must use the load-balanced durable agent control endpoint")


def validate_static_workspace(documents: list[tuple[str, str, str]], errors: list[str]) -> None:
    runtime = next((doc for kind, name, doc in documents
                    if kind == 'Deployment' and name == 'weknora-agent-runtime'), '')
    for name, value in {
        'AGENT_WORKSPACE_PVC_NAME': 'agent-runtime-shared',
        'AGENT_WORKSPACE_PV_NAME': 'weknora-agent-runtime-pv',
        'AGENT_WORKSPACE_NAMESPACE': 'weknora-agent-workspaces',
        'AGENT_WORKSPACE_IMAGE_PULL_SECRETS': 'default-secret',
    }.items():
        if not has_env(runtime, name, value):
            errors.append(f'static workspace requires {name}={value}')
    if 'AGENT_WORKSPACE_STORAGE_SIZE' in runtime or 'AGENT_WORKSPACE_STORAGE_CLASS' in runtime:
        errors.append('retired per-run PVC configuration remains in runtime')
    if not has_env(runtime, 'AGENT_WORKSPACE_NODE_SELECTOR', r'{\"kubernetes.io/hostname\":\"10.14.201.2\"}'):
        errors.append('static workspace must select local PV node 10.14.201.2')
    role = next((doc for kind, name, doc in documents if kind == 'Role' and name == 'weknora-agent-runtime'), '')
    expected = r'resources:\s*\[persistentvolumeclaims\]\s*\n\s*resourceNames:\s*\["agent-runtime-shared"\]\s*\n\s*verbs:\s*\[get\]'
    if not re.search(expected, role):
        errors.append('static workspace Role must grant only get on the shared PVC')
    if 'resources: [pods, configmaps]' not in role:
        errors.append('workspace lifecycle ConfigMap permissions are missing')
    expected_namespaces = {
        ('ServiceAccount', 'weknora-agent-runtime'): 'weknora',
        ('Deployment', 'weknora-agent-runtime'): 'weknora',
        ('Role', 'weknora-agent-runtime'): 'weknora-agent-workspaces',
        ('RoleBinding', 'weknora-agent-runtime'): 'weknora-agent-workspaces',
        ('NetworkPolicy', 'workspace-isolation'): 'weknora-agent-workspaces',
    }
    objects = {(kind, name): doc for kind, name, doc in documents}
    for identity, expected_namespace in expected_namespaces.items():
        document = objects.get(identity)
        if not document:
            errors.append(f'missing required namespaced object {identity[0]}/{identity[1]}')
            continue
        actual_namespace = object_namespace(document)
        if actual_namespace != expected_namespace:
            errors.append(
                f'{identity[0]}/{identity[1]} namespace={actual_namespace!r}, '
                f'expected {expected_namespace!r}'
            )
    policy = next((doc for kind, name, doc in documents
                   if kind == 'NetworkPolicy' and name == 'workspace-isolation'), '')
    if not policy:
        errors.append('workspace ingress isolation NetworkPolicy is missing')
    else:
        if not re.search(r'(?m)^  policyTypes: \[Ingress\]\s*$', policy):
            errors.append('workspace NetworkPolicy must select ingress only')
        if not re.search(r'(?m)^  ingress: \[\]\s*$', policy):
            errors.append('workspace NetworkPolicy must deny all ingress')
        if re.search(r'(?m)^  egress:', policy):
            errors.append('workspace NetworkPolicy must leave egress unrestricted')


def validate_workloads(
    documents: list[tuple[str, str, str]], mode: str, errors: list[str]
) -> None:
    validate_static_workspace(documents, errors)
    kinds = {kind for kind, _, _ in documents}
    # Both workload sets stay dark. The reviewed cutoff Ingress is restored
    # only after platform, rebuild and business smoke verification succeeds.
    forbidden = {
        "Ingress", "Job", "Secret", "StatefulSet",
        "PersistentVolume", "PersistentVolumeClaim",
    }
    for kind in sorted(kinds & forbidden):
        errors.append(f"{mode} workload render contains forbidden kind {kind}")

    deployments = {
        name: document
        for kind, name, document in documents
        if kind == "Deployment"
    }
    if set(deployments) != set(EXPECTED_NORMAL_REPLICAS):
        errors.append(
            "deployment set differs from the production topology: "
            f"actual={sorted(deployments)} expected={sorted(EXPECTED_NORMAL_REPLICAS)}"
        )
    for name, target in EXPECTED_NORMAL_REPLICAS.items():
        document = deployments.get(name)
        if document is None:
            continue
        raw_replicas = scalar(document, "replicas", 2)
        if raw_replicas is None or not raw_replicas.isdigit():
            errors.append(f"{name} has no integer spec.replicas")
            continue
        expected = 0 if mode == "gated" and name in SUSPENDED_DEPLOYMENTS else target
        if int(raw_replicas) != expected:
            errors.append(f"{mode} {name} replicas={raw_replicas}, expected {expected}")
        for env_name, env_value in EXPECTED_ROLE_ENV.get(name, {}).items():
            if not has_env(document, env_name, env_value):
                errors.append(
                    f"{name} does not pin {env_name}={env_value} from the measured capacity plan"
                )
        if name == "weknora-app":
            validate_agent_callback(document, errors)

    pdb_names = {name for kind, name, _ in documents if kind == "PodDisruptionBudget"}
    if mode == "gated":
        unexpected = sorted(pdb_names & SUSPENDED_DEPLOYMENTS)
        if unexpected:
            errors.append(f"suspended deployments still have PDBs: {unexpected}")
    else:
        runtime_pdb = next(
            (
                document
                for kind, name, document in documents
                if kind == "PodDisruptionBudget" and name == "weknora-agent-runtime"
            ),
            "",
        )
        if not runtime_pdb:
            errors.append("normal manifest is missing agent-runtime PodDisruptionBudget")
        elif object_namespace(runtime_pdb) != "weknora":
            errors.append("agent-runtime PodDisruptionBudget must be in namespace weknora")

    raw = "\n".join(document for _, _, document in documents)
    actual_paths = set(
        re.findall(
            r'(?m)^\s+path:\s*"?(/mnt/weknora-data/weknora-v2-scratch/[^\s"]+)"?\s*$',
            raw,
        )
    )
    missing_paths = sorted(EXPECTED_SCRATCH_PATHS - actual_paths)
    if missing_paths:
        errors.append(f"missing production scratch hostPaths: {missing_paths}")
    for path in EXPECTED_SCRATCH_PATHS:
        pattern = rf'path:\s*"?{re.escape(path)}"?\s*\n\s*type:\s*Directory\s*$'
        if not re.search(pattern, raw, re.MULTILINE):
            errors.append(f"scratch hostPath is not fail-closed Directory: {path}")


def validate_migration(
    documents: list[tuple[str, str, str]], release_id: str, errors: list[str]
) -> None:
    if len(documents) != 1 or documents[0][0] != "Job":
        errors.append("migration render must contain exactly one Job")
        return
    _, name, document = documents[0]
    normalized = release_id.lower().replace("_", "-").replace(".", "-")
    if normalized not in name:
        errors.append(f"migration Job {name} does not contain release id {normalized}")
    if scalar(document, "backoffLimit", 2) != "0":
        errors.append("migration Job backoffLimit must be 0")
    if "AUTO_MIGRATE" not in document or 'value: "true"' not in document:
        errors.append("migration Job does not explicitly enable migration")
    if "/mnt/weknora-data/weknora-v2-scratch/migration" not in document:
        errors.append("migration Job does not use its isolated scratch directory")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("normal", "gated", "migration"))
    parser.add_argument("manifest", help="manifest path, or - to read standard input")
    parser.add_argument("--release-id", default="")
    args = parser.parse_args()
    errors: list[str] = []
    try:
        documents = parse_documents(args.manifest)
        validate_images(documents, errors)
        if args.mode == "migration":
            if not args.release_id:
                errors.append("--release-id is required for migration mode")
            else:
                validate_migration(documents, args.release_id, errors)
        else:
            validate_workloads(documents, args.mode, errors)
    except (OSError, ValueError) as exc:
        errors.append(str(exc))
    if errors:
        for error in errors:
            print(f"ERROR: {error}")
        return 1
    print(f"OK: {args.mode} manifest passed release topology validation")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
