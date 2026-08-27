[CmdletBinding()]
param(
    [ValidateSet("up", "down", "rebuild", "status")]
    [string]$Action = "up",
    [ValidateSet("eval", "main")]
    [string]$Target = "eval",
    [string]$MainWorktree = "C:\weknora",
    [switch]$RebuildBase
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..\..\..")).Path
$mainEnv = Join-Path $MainWorktree ".env"
$evalEnv = Join-Path $PSScriptRoot "eval.env"
$runtimeCompose = Join-Path $repoRoot "custom\tests\runtime_profile_e2e\docker-compose.yml"
$infraCompose = Join-Path $repoRoot "docker-compose.dev.yml"
$agentCompose = Join-Path $repoRoot "custom\docker-compose.general-agent.yml"
$platformCompose = Join-Path $PSScriptRoot "docker-compose.yml"

function Invoke-Docker {
    param(
        [Parameter(Mandatory)] [string[]]$DockerArgs,
        [switch]$AllowFailure
    )
    & docker @DockerArgs
    if ($LASTEXITCODE -ne 0 -and -not $AllowFailure) {
        throw "docker command failed with exit code $LASTEXITCODE"
    }
}

function Assert-DockerReady {
    Invoke-Docker -DockerArgs @("version", "--format", "{{.Server.Version}}")
}

function Stop-WorktreeFrontend {
    param(
        [Parameter(Mandatory)] [string]$Worktree,
        [Parameter(Mandatory)] [string]$Label
    )
    if (-not (Test-Path -LiteralPath $Worktree)) {
        return
    }

    $frontendRoot = [IO.Path]::GetFullPath((Join-Path $Worktree "frontend")).TrimEnd("\")
    $frontendPattern = [regex]::Escape($frontendRoot)
    $processes = @(
        Get-CimInstance Win32_Process -ErrorAction SilentlyContinue |
            Where-Object {
                $_.CommandLine -and
                $_.CommandLine -match $frontendPattern -and
                $_.Name -match "^(node|esbuild)(\.exe)?$"
            }
    )
    foreach ($process in ($processes | Sort-Object ProcessId -Descending)) {
        Stop-Process -Id $process.ProcessId -Force -ErrorAction SilentlyContinue
    }
    if ($processes.Count -gt 0) {
        Write-Host "$Label frontend stopped: $($processes.ProcessId -join ', ')"
    }
}

function Eval-ComposePrefix {
    param([string]$Kind)
    $common = @("compose", "--env-file", $mainEnv, "--env-file", $evalEnv)
    switch ($Kind) {
        "infra" { return $common + @("-p", "weknora-agent-eval-infra", "-f", $infraCompose) }
        "agents" { return $common + @("-p", "weknora-agent-eval-agents", "-f", $agentCompose) }
        "runtime" { return $common + @("-p", "weknora-agent-eval-runtime", "-f", $runtimeCompose) }
        "platform" { return $common + @("-p", "weknora-agent-eval-platform", "-f", $platformCompose) }
        default { throw "unknown eval compose kind: $Kind" }
    }
}

function Main-ComposePrefix {
    param([string]$Kind)
    $mainRootResolved = (Resolve-Path $MainWorktree).Path
    $common = @("compose", "--env-file", (Join-Path $mainRootResolved ".env"))
    switch ($Kind) {
        "infra" { return $common + @("-p", "weknora", "-f", (Join-Path $mainRootResolved "docker-compose.dev.yml")) }
        "agents" { return $common + @("-p", "weknora", "-f", (Join-Path $mainRootResolved "custom\docker-compose.general-agent.yml")) }
        "runtime" { return $common + @("-p", "weknora-runtime-profile-e2e", "-f", (Join-Path $mainRootResolved "custom\tests\runtime_profile_e2e\docker-compose.yml")) }
        default { throw "unknown main compose kind: $Kind" }
    }
}

function Stop-EvalStack {
    Stop-WorktreeFrontend -Worktree $repoRoot -Label "Eval"
    foreach ($kind in @("runtime", "platform", "agents")) {
        Invoke-Docker -DockerArgs ((Eval-ComposePrefix $kind) + @("stop")) -AllowFailure
    }
    Invoke-Docker -DockerArgs ((Eval-ComposePrefix "infra") + @(
        "--profile", "full", "stop"
    )) -AllowFailure
}

function Stop-MainStack {
    if (-not (Test-Path -LiteralPath $MainWorktree)) {
        return
    }
    Stop-WorktreeFrontend -Worktree $MainWorktree -Label "Main"
    foreach ($kind in @("runtime", "agents")) {
        Invoke-Docker -DockerArgs ((Main-ComposePrefix $kind) + @("stop")) -AllowFailure
    }
    Invoke-Docker -DockerArgs ((Main-ComposePrefix "infra") + @(
        "--profile", "full", "stop"
    )) -AllowFailure
}

function Ensure-EvalBaseImage {
    $needsBuild = [bool]$RebuildBase
    & docker image inspect "weknora-agent-eval-dev-app:local" *> $null
    if ($LASTEXITCODE -ne 0) {
        & docker image inspect "weknora-dev-app:local" *> $null
        if ($LASTEXITCODE -eq 0 -and -not $RebuildBase) {
            # This image contains only immutable development dependencies. An
            # eval-specific tag keeps mutable application images isolated while
            # Docker deduplicates the identical read-only layers.
            Invoke-Docker -DockerArgs @(
                "tag", "weknora-dev-app:local", "weknora-agent-eval-dev-app:local"
            )
        } else {
            $needsBuild = $true
        }
    }
    if ($needsBuild) {
        Invoke-Docker -DockerArgs @(
            "build", "-f", (Join-Path $repoRoot "docker\Dockerfile.dev-app"),
            "-t", "weknora-agent-eval-dev-app:local", $repoRoot
        )
    }
}

function Start-EvalStack {
    Stop-MainStack
    Ensure-EvalBaseImage
    & docker volume inspect "weknora-agent-eval-go-mod-cache" *> $null
    if ($LASTEXITCODE -ne 0) {
        Invoke-Docker -DockerArgs @("volume", "create", "weknora-agent-eval-go-mod-cache")
    }

    $infraArgs = (Eval-ComposePrefix "infra") + @(
        "up", "-d", "--wait", "postgres", "redis", "minio", "neo4j", "docreader"
    )
    if ($Action -eq "rebuild") {
        $infraArgs = (Eval-ComposePrefix "infra") + @(
            "up", "-d", "--wait", "--build", "postgres", "redis", "minio", "neo4j", "docreader"
        )
    }
    Invoke-Docker -DockerArgs $infraArgs

    if ($Action -eq "rebuild") {
        # Build the two Python agents serially so Docker does not saturate CPU,
        # memory and disk on a development laptop.
        Invoke-Docker -DockerArgs ((Eval-ComposePrefix "agents") + @(
            "build", "weknora-custom-general-agent"
        ))
        Invoke-Docker -DockerArgs ((Eval-ComposePrefix "agents") + @(
            "build", "weknora-custom-document-processing-agent"
        ))
    }
    Invoke-Docker -DockerArgs ((Eval-ComposePrefix "agents") + @("up", "-d"))

    Invoke-Docker -DockerArgs ((Eval-ComposePrefix "platform") + @("up", "-d", "--wait"))

    $runtimePrefix = Eval-ComposePrefix "runtime"
    Invoke-Docker -DockerArgs ($runtimePrefix + @("stop")) -AllowFailure
    Invoke-Docker -DockerArgs ($runtimePrefix + @("build", "runtime-api-1"))
    # Provisioning is fail-closed: serving roles are started only after the
    # dedicated one-shot migration role exits successfully.
    Invoke-Docker -DockerArgs ($runtimePrefix + @(
        "up", "--force-recreate", "--abort-on-container-exit",
        "--exit-code-from", "migration", "migration"
    ))
    Invoke-Docker -DockerArgs ($runtimePrefix + @(
        "up", "-d", "--force-recreate", "--wait", "--wait-timeout", "180",
        "runtime-api-1", "runtime-api-2", "runtime-api-3",
        "runtime-parse-1", "runtime-parse-2",
        "runtime-derivative-1", "runtime-derivative-2",
        "runtime-wiki-1", "runtime-wiki-2",
        "runtime-maintenance-1", "runtime-maintenance-2",
        "runtime-docreader-2", "runtime-docreader-3",
        "runtime-docreader-entry", "runtime-entry"
    ))
}

function Start-MainStack {
    Stop-EvalStack
    $mainRootResolved = (Resolve-Path $MainWorktree).Path
    $infraArgs = (Main-ComposePrefix "infra") + @(
        "up", "-d", "--wait", "postgres", "redis", "minio", "neo4j", "docreader"
    )
    Invoke-Docker -DockerArgs $infraArgs
    if ($Action -eq "rebuild") {
        Invoke-Docker -DockerArgs ((Main-ComposePrefix "agents") + @(
            "build", "weknora-custom-general-agent"
        ))
        Invoke-Docker -DockerArgs ((Main-ComposePrefix "agents") + @(
            "build", "weknora-custom-document-processing-agent"
        ))
    }
    Invoke-Docker -DockerArgs ((Main-ComposePrefix "agents") + @("up", "-d"))
    $runtimePrefix = Main-ComposePrefix "runtime"
    Invoke-Docker -DockerArgs ($runtimePrefix + @("stop")) -AllowFailure
    if ($Action -eq "rebuild") {
        Invoke-Docker -DockerArgs ($runtimePrefix + @("build", "runtime-api-1"))
    }
    Invoke-Docker -DockerArgs ($runtimePrefix + @("up", "-d", "--force-recreate"))
    Write-Host "Main worktree active: $mainRootResolved"
}

Assert-DockerReady
if ($Action -eq "status") {
    Invoke-Docker -DockerArgs @("ps", "--format", "table {{.Names}}\t{{.Status}}\t{{.Ports}}")
    exit 0
}
if ($Action -eq "down") {
    if ($Target -eq "eval") { Stop-EvalStack } else { Stop-MainStack }
    exit 0
}
if ($Target -eq "eval") {
    Start-EvalStack
    Write-Host "Eval worktree active: API http://localhost:18080, Langfuse http://localhost:13001"
} else {
    Start-MainStack
    Write-Host "Main worktree active: API http://localhost:8080"
}
