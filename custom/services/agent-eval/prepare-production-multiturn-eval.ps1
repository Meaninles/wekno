[CmdletBinding()]
param(
    [ValidateSet("experiment", "optimization", "release", "sealed")]
    [string]$Stage = "release",
    [switch]$SkipKnowledgeBaseRefresh
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

if (-not $SkipKnowledgeBaseRefresh) {
    & (Join-Path $PSScriptRoot "prepare-production-derived-kbs.ps1")
    if ($LASTEXITCODE -ne 0) {
        throw "production-derived knowledge-base preparation failed with exit code $LASTEXITCODE"
    }
}

& (Join-Path $PSScriptRoot "validate-production-corpus-anchors.ps1")
if ($LASTEXITCODE -ne 0) {
    throw "production-derived corpus anchor validation failed with exit code $LASTEXITCODE"
}

$common = @{
    Dataset = "/workspace/datasets/production-multiturn-ready.v1.jsonl"
    Profiles = "/workspace/profiles/production-derived-multiturn.v1.json"
    PreflightOnly = $true
}

switch ($Stage) {
    "experiment" {
        $common.Split = "dev"
        $common.Manifest = "/workspace/manifests/production-multiturn-ready.v1-experiment-evaluator-v4.manifest.json"
        $common.Policy = "/workspace/policies/production-multiturn-experiment-gate.v1.json"
    }
    "optimization" {
        $common.Split = "dev"
        $common.Manifest = "/workspace/manifests/production-multiturn-ready.v1-optimization-evaluator-v4.manifest.json"
        $common.Policy = "/workspace/policies/production-multiturn-optimization-gate.v1.json"
    }
    "release" {
        $common.Split = "gate"
        $common.Manifest = "/workspace/manifests/production-multiturn-ready.v1-release-evaluator-v4.manifest.json"
        $common.Policy = "/workspace/policies/production-multiturn-release-gate.v1.json"
    }
    "sealed" {
        $common.Split = "sealed_holdout"
        $common.Dataset = "/workspace/sealed/production-multiturn-holdout.v1.jsonl"
        $common.Manifest = "/workspace/manifests/production-multiturn-holdout.v1-evaluator-v4.manifest.json"
        $common.Policy = "/workspace/policies/production-multiturn-sealed-gate.v1.json"
        $common.AllowSealed = $true
    }
}

& (Join-Path $PSScriptRoot "eval-loop.ps1") @common
if ($LASTEXITCODE -ne 0) {
    throw "production-derived eval preflight failed with exit code $LASTEXITCODE"
}

Write-Host "PRODUCTION_MULTITURN_EVAL_READY stage=$Stage formal_eval_executed=false"
