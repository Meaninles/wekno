[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$runnerEnv = Join-Path $PSScriptRoot "runner.env"
$evalPostgres = "WeKnora-agent-eval-postgres-dev"

if (-not (Test-Path -LiteralPath $runnerEnv)) {
    throw "runner.env is required; run prepare-runner-env.ps1 first"
}

$settings = @{}
Get-Content -LiteralPath $runnerEnv | ForEach-Object {
    if ($_ -match '^(?<key>[A-Z0-9_]+)=(?<value>.*)$') {
        $settings[$Matches.key] = $Matches.value
    }
}
$modelID = [string]$settings["AGENT_EVAL_SUMMARY_MODEL_ID"]
if ($modelID -notmatch '^[A-Za-z0-9._:-]+$') {
    throw "AGENT_EVAL_SUMMARY_MODEL_ID is missing or unsafe"
}

$mainContainers = @(
    & docker ps --format "{{.Names}}" |
        Where-Object {
            $_ -notmatch '(?i)agent-eval' -and
            ($_ -match '(?i)^WeKnora-' -or $_ -match '(?i)^weknora-runtime-profile-e2e-' -or $_ -match '(?i)^weknora-custom-')
        }
)
if ($mainContainers.Count -gt 0) {
    throw "Main WeKnora services must be stopped before preparing discovery: $($mainContainers -join ', ')"
}

$running = & docker inspect -f "{{.State.Running}}" $evalPostgres 2>$null
if ($LASTEXITCODE -ne 0 -or $running -ne "true") {
    throw "$evalPostgres is not running"
}

$sql = @"
UPDATE custom_agents
SET config = jsonb_set(config, '{model_id}', to_jsonb('$modelID'::text), true),
    updated_at = CURRENT_TIMESTAMP
WHERE tenant_id = 10000
  AND id IN ('builtin-quick-answer', 'builtin-smart-reasoning', 'builtin-general-agent');

UPDATE custom_agents
SET config = jsonb_set(config, '{history_turns}', to_jsonb(10), true),
    updated_at = CURRENT_TIMESTAMP
WHERE tenant_id = 10000
  AND id = 'builtin-smart-reasoning';

SELECT id,
       config->>'model_id' AS model_id,
       config->>'history_turns' AS history_turns
FROM custom_agents
WHERE tenant_id = 10000
  AND id IN ('builtin-quick-answer', 'builtin-smart-reasoning', 'builtin-general-agent')
ORDER BY id;
"@

$sql | & docker exec -i $evalPostgres sh -lc 'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -P pager=off'
if ($LASTEXITCODE -ne 0) {
    throw "failed to configure isolated multi-turn discovery profiles"
}

Write-Host "Prepared isolated discovery profiles. No main-worktree service or data was changed."
