[CmdletBinding()]
param(
    [string]$MainWorktree = "C:\weknora",
    [int]$MinimumFreeGB = 20,
    [ValidateRange(1, 4)]
    [int]$PostgresJobs = 2,
    [switch]$FullMinio,
    [switch]$SkipRedis,
    [switch]$SkipNeo4j,
    [switch]$KeepSnapshot,
    [switch]$Force
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..\..\..")).Path
$mainRoot = (Resolve-Path $MainWorktree).Path
$mainEnvPath = Join-Path $mainRoot ".env"
$evalEnvPath = Join-Path $PSScriptRoot "eval.env"
$artifactDir = Join-Path $PSScriptRoot "artifacts"
$timestamp = Get-Date -Format "yyyyMMdd-HHmmss"
$snapshotRoot = Join-Path ([System.IO.Path]::GetTempPath()) "weknora-agent-eval-seed-$timestamp"

function Invoke-Docker {
    param([Parameter(Mandatory)] [string[]]$DockerArgs, [switch]$AllowFailure)
    & docker @DockerArgs
    if ($LASTEXITCODE -ne 0 -and -not $AllowFailure) {
        throw "docker command failed with exit code $LASTEXITCODE"
    }
}

function Read-DotEnv {
    param([string]$Path)
    $values = @{}
    foreach ($line in Get-Content -LiteralPath $Path) {
        $trimmed = $line.Trim()
        if (-not $trimmed -or $trimmed.StartsWith("#") -or -not $trimmed.Contains("=")) { continue }
        $parts = $trimmed.Split("=", 2)
        $name = $parts[0].Trim()
        $value = $parts[1].Trim().Trim('"').Trim("'")
        if ($name) { $values[$name] = $value }
    }
    return $values
}

function Value-OrDefault {
    param([hashtable]$Values, [string]$Name, [string]$Default)
    if ($Values.ContainsKey($Name) -and [string]$Values[$Name]) { return [string]$Values[$Name] }
    return $Default
}

function Main-InfraPrefix {
    return @(
        "compose", "--env-file", $mainEnvPath, "-p", "weknora",
        "-f", (Join-Path $mainRoot "docker-compose.dev.yml")
    )
}

function Eval-InfraPrefix {
    return @(
        "compose", "--env-file", $mainEnvPath, "--env-file", $evalEnvPath,
        "-p", "weknora-agent-eval-infra", "-f", (Join-Path $repoRoot "docker-compose.dev.yml")
    )
}

function Get-ContainerBytes {
    param([string]$Container, [string]$Path)
    $raw = & docker exec $Container sh -ec "du -sb '$Path' 2>/dev/null | cut -f1"
    if ($LASTEXITCODE -ne 0 -or -not $raw) { return [int64]0 }
    $parsed = [int64]0
    if ([int64]::TryParse(($raw | Select-Object -First 1).Trim(), [ref]$parsed)) { return $parsed }
    return [int64]0
}

function Get-MinioPrefixBytes {
    param(
        [string]$SourceContainer,
        [string]$SourceNetwork,
        [string]$AccessKey,
        [string]$SecretKey,
        [string]$Bucket,
        [string]$Prefix
    )
    $normalized = $Prefix.Trim('/')
    $source = if ($normalized) { "source/$Bucket/$normalized" } else { "source/$Bucket" }
    $script = "mc alias set source http://$SourceContainer`:9000 `"`$MINIO_ACCESS_KEY`" `"`$MINIO_SECRET_KEY`" >/dev/null; mc du --json '$source'"
    $output = & docker run --rm --network $SourceNetwork `
        -e "MINIO_ACCESS_KEY=$AccessKey" -e "MINIO_SECRET_KEY=$SecretKey" `
        --entrypoint /bin/sh minio/mc:RELEASE.2025-08-13T08-35-41Z -ec $script
    if ($LASTEXITCODE -ne 0) {
        throw "failed to measure MinIO prefix: $source"
    }
    $bytes = [int64]0
    $sawSize = $false
    foreach ($line in $output) {
        try {
            $row = $line | ConvertFrom-Json
            $totalSizeProperty = $row.PSObject.Properties["totalSize"]
            $sizeProperty = $row.PSObject.Properties["size"]
            if ($null -ne $totalSizeProperty) {
                $bytes += [int64]$totalSizeProperty.Value
                $sawSize = $true
            } elseif ($null -ne $sizeProperty) {
                $bytes += [int64]$sizeProperty.Value
                $sawSize = $true
            }
        } catch {
            # mc can emit a non-JSON progress line before the final JSON row.
        }
    }
    if (-not $sawSize) { throw "mc returned no size for MinIO prefix: $source" }
    return $bytes
}

function Assert-Capacity {
    param([int64]$EstimatedSourceBytes)
    $driveName = ([System.IO.Path]::GetPathRoot($repoRoot)).TrimEnd('\').TrimEnd(':')
    $drive = Get-PSDrive -Name $driveName
    $minimumBytes = [int64]$MinimumFreeGB * 1GB
    # Target volumes plus temporary portable exports coexist during restore.
    # 1.5x is conservative for compressed Postgres/Redis exports while MinIO
    # object files are copied without relying on compression.
    $requiredBytes = [int64]([math]::Ceiling($EstimatedSourceBytes * 1.5)) + $minimumBytes
    if ($drive.Free -lt $requiredBytes -and -not $Force) {
        throw "capacity preflight failed: free=$([math]::Round($drive.Free / 1GB, 1))GB, required=$([math]::Round($requiredBytes / 1GB, 1))GB. Reduce MinIO scope or rerun with -Force after manual verification."
    }
}

function Export-MinioPrefix {
    param(
        [string]$SourceContainer,
        [string]$SourceNetwork,
        [string]$AccessKey,
        [string]$SecretKey,
        [string]$Bucket,
        [string]$Prefix,
        [string]$DestinationRoot
    )
    $normalized = $Prefix.Trim('/')
    $destination = if ($normalized) { "/backup/minio/$Bucket/$normalized" } else { "/backup/minio/$Bucket" }
    $source = if ($normalized) { "source/$Bucket/$normalized" } else { "source/$Bucket" }
    $script = "mc alias set source http://$SourceContainer`:9000 `"`$MINIO_ACCESS_KEY`" `"`$MINIO_SECRET_KEY`" >/dev/null; mkdir -p '$destination'; mc --quiet mirror --overwrite --preserve '$source' '$destination'"
    Invoke-Docker -DockerArgs @(
        "run", "--rm", "--network", $SourceNetwork,
        "-e", "MINIO_ACCESS_KEY=$AccessKey", "-e", "MINIO_SECRET_KEY=$SecretKey",
        "-v", "${DestinationRoot}:/backup", "--entrypoint", "/bin/sh",
        "minio/mc:RELEASE.2025-08-13T08-35-41Z", "-ec", $script
    )
}

function Restore-MinioPrefix {
    param(
        [string]$TargetContainer,
        [string]$TargetNetwork,
        [string]$AccessKey,
        [string]$SecretKey,
        [string]$Bucket,
        [string]$Prefix,
        [string]$SourceRoot
    )
    $normalized = $Prefix.Trim('/')
    $source = if ($normalized) { "/backup/minio/$Bucket/$normalized" } else { "/backup/minio/$Bucket" }
    $destination = if ($normalized) { "target/$Bucket/$normalized" } else { "target/$Bucket" }
    $script = "mc alias set target http://$TargetContainer`:9000 `"`$MINIO_ACCESS_KEY`" `"`$MINIO_SECRET_KEY`" >/dev/null; mc mb --ignore-existing target/$Bucket >/dev/null; if [ -d '$source' ]; then mc --quiet mirror --overwrite --preserve '$source' '$destination'; fi"
    Invoke-Docker -DockerArgs @(
        "run", "--rm", "--network", $TargetNetwork,
        "-e", "MINIO_ACCESS_KEY=$AccessKey", "-e", "MINIO_SECRET_KEY=$SecretKey",
        "-v", "${SourceRoot}:/backup:ro", "--entrypoint", "/bin/sh",
        "minio/mc:RELEASE.2025-08-13T08-35-41Z", "-ec", $script
    )
}

function Remove-VerifiedSnapshot {
    param([string]$Path)
    $resolved = (Resolve-Path -LiteralPath $Path).Path
    $tempRoot = (Resolve-Path -LiteralPath ([System.IO.Path]::GetTempPath())).Path.TrimEnd('\')
    if (-not $resolved.StartsWith($tempRoot + '\', [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "refusing to remove snapshot outside temporary directory: $resolved"
    }
    Remove-Item -LiteralPath $resolved -Recurse -Force
}

& docker version --format "{{.Server.Version}}" | Out-Null
if ($LASTEXITCODE -ne 0) { throw "Docker Desktop is not running" }

$mainValues = Read-DotEnv $mainEnvPath
$dbUser = Value-OrDefault $mainValues "DB_USER" "postgres"
$dbPassword = Value-OrDefault $mainValues "DB_PASSWORD" "postgres"
$dbName = Value-OrDefault $mainValues "DB_NAME" "WeKnora"
$redisPassword = Value-OrDefault $mainValues "REDIS_PASSWORD" ""
$neo4jUser = Value-OrDefault $mainValues "NEO4J_USERNAME" "neo4j"
$neo4jPassword = Value-OrDefault $mainValues "NEO4J_PASSWORD" "password"
$minioAccessKey = Value-OrDefault $mainValues "MINIO_ACCESS_KEY_ID" "minioadmin"
$minioSecretKey = Value-OrDefault $mainValues "MINIO_SECRET_ACCESS_KEY" "minioadmin"
$minioBucket = Value-OrDefault $mainValues "MINIO_BUCKET_NAME" "weknora-original-inputs"
$mainNetwork = "weknora_WeKnora-network-dev"
$evalNetwork = "weknora-agent-eval-network"

# Stop both application planes first. Only the source data services are brought
# up during export; after export they are stopped before target restore starts.
& (Join-Path $PSScriptRoot "stack.ps1") -Action down -Target eval -MainWorktree $mainRoot
& (Join-Path $PSScriptRoot "stack.ps1") -Action down -Target main -MainWorktree $mainRoot
Invoke-Docker -DockerArgs ((Main-InfraPrefix) + @(
    "up", "-d", "--wait", "postgres", "redis", "minio", "neo4j"
))

$minioPrefixes = @()
if ($FullMinio) {
    $minioPrefixes = @((""))
} else {
    $defaultMinioPrefixes = [ordered]@{
        MINIO_PATH_PREFIX = "weknora/__weknora_private_knowledge_objects_v1__/deployment/dev-local/namespace/74b3d025-5a14-4a6d-b0fc-ff228d0ba98c/"
        CUSTOM_GENERAL_AGENT_ARTIFACT_PATH_PREFIX = "weknora/__weknora_private_agent_artifacts_v1__/deployment/dev-local/namespace/74b3d025-5a14-4a6d-b0fc-ff228d0ba98c/"
        CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_PATH_PREFIX = "weknora/__weknora_claude_sdk_original_inputs_v1__/deployment/dev-local/namespace/74b3d025-5a14-4a6d-b0fc-ff228d0ba98c/"
        CUSTOM_SKILLHUB_PROFESSIONAL_PATH_PREFIX = "weknora/__weknora_private_professional_skills_v1__/deployment/dev-local/namespace/74b3d025-5a14-4a6d-b0fc-ff228d0ba98c/"
    }
    foreach ($name in $defaultMinioPrefixes.Keys) {
        $value = Value-OrDefault $mainValues $name $defaultMinioPrefixes[$name]
        if ($value) { $minioPrefixes += $value }
    }
    $minioPrefixes = @($minioPrefixes | Sort-Object -Unique)
    if (-not $minioPrefixes) {
        throw "no MinIO prefixes found in main .env; use -FullMinio only after capacity review"
    }
}

$minioBytes = [int64]0
foreach ($prefix in $minioPrefixes) {
    $minioBytes += Get-MinioPrefixBytes "WeKnora-minio-dev" $mainNetwork $minioAccessKey $minioSecretKey $minioBucket $prefix
}

$sourceSizes = [ordered]@{
    postgres = Get-ContainerBytes "WeKnora-postgres-dev" "/var/lib/postgresql/data"
    redis = Get-ContainerBytes "WeKnora-redis-dev" "/data"
    minio_selected_prefixes = $minioBytes
    neo4j = Get-ContainerBytes "WeKnora-neo4j-dev" "/data"
}
$estimatedBytes = [int64](($sourceSizes.Values | Measure-Object -Sum).Sum)
Assert-Capacity $estimatedBytes
New-Item -ItemType Directory -Path $snapshotRoot -Force | Out-Null
$seedSucceeded = $false

try {
    if (-not $SkipRedis) {
        $redisDump = Join-Path $snapshotRoot "redis.rdb"
        Invoke-Docker -DockerArgs @(
            "exec", "-e", "REDISCLI_AUTH=$redisPassword", "WeKnora-redis-dev",
            "redis-cli", "--rdb", "/tmp/agent-eval-seed.rdb"
        )
        Invoke-Docker -DockerArgs @("cp", "WeKnora-redis-dev:/tmp/agent-eval-seed.rdb", $redisDump)
        Invoke-Docker -DockerArgs @("exec", "WeKnora-redis-dev", "rm", "-f", "/tmp/agent-eval-seed.rdb")
    }

    if (-not $SkipNeo4j) {
        Invoke-Docker -DockerArgs @("exec", "WeKnora-neo4j-dev", "rm", "-f", "/var/lib/neo4j/import/agent-eval-seed.cypher")
        Invoke-Docker -DockerArgs @(
            "exec", "WeKnora-neo4j-dev", "cypher-shell", "-u", $neo4jUser, "-p", $neo4jPassword,
            "CALL apoc.export.cypher.all('agent-eval-seed.cypher',{format:'cypher-shell'}) YIELD file RETURN file;"
        )
        Invoke-Docker -DockerArgs @("cp", "WeKnora-neo4j-dev:/var/lib/neo4j/import/agent-eval-seed.cypher", (Join-Path $snapshotRoot "neo4j.cypher"))
        Invoke-Docker -DockerArgs @("exec", "WeKnora-neo4j-dev", "rm", "-f", "/var/lib/neo4j/import/agent-eval-seed.cypher")
    }

    foreach ($prefix in $minioPrefixes) {
        Export-MinioPrefix "WeKnora-minio-dev" $mainNetwork $minioAccessKey $minioSecretKey $minioBucket $prefix $snapshotRoot
    }

    # Directory format permits a synchronized parallel dump/restore. Keep the
    # default at two workers so the laptop stays responsive during this one-off
    # clone while avoiding the previous single-core bottleneck.
    $postgresDump = Join-Path $snapshotRoot "weknora-pgdump"
    New-Item -ItemType Directory -Path $postgresDump -Force | Out-Null
    Invoke-Docker -DockerArgs @("exec", "WeKnora-postgres-dev", "rm", "-rf", "/tmp/agent-eval-seed-db")
    Invoke-Docker -DockerArgs @(
        "exec", "-e", "PGPASSWORD=$dbPassword", "WeKnora-postgres-dev",
        "pg_dump", "-U", $dbUser, "-d", $dbName, "-Fd", "-j", [string]$PostgresJobs,
        "--compress=gzip:6", "--no-owner", "-f", "/tmp/agent-eval-seed-db"
    )
    Invoke-Docker -DockerArgs @("cp", "WeKnora-postgres-dev:/tmp/agent-eval-seed-db/.", $postgresDump)
    Invoke-Docker -DockerArgs @("exec", "WeKnora-postgres-dev", "rm", "-rf", "/tmp/agent-eval-seed-db")

    Invoke-Docker -DockerArgs ((Main-InfraPrefix) + @("--profile", "full", "stop"))
    Invoke-Docker -DockerArgs ((Eval-InfraPrefix) + @(
        "up", "-d", "--wait", "postgres", "redis", "minio", "neo4j"
    ))

    Invoke-Docker -DockerArgs @("exec", "WeKnora-agent-eval-postgres-dev", "mkdir", "-p", "/tmp/agent-eval-seed-db")
    Invoke-Docker -DockerArgs @("cp", (Join-Path $postgresDump "."), "WeKnora-agent-eval-postgres-dev:/tmp/agent-eval-seed-db")
    Invoke-Docker -DockerArgs @(
        "exec", "-e", "PGPASSWORD=$dbPassword", "WeKnora-agent-eval-postgres-dev",
        "dropdb", "-U", $dbUser, "--if-exists", "--force", $dbName
    )
    Invoke-Docker -DockerArgs @(
        "exec", "-e", "PGPASSWORD=$dbPassword", "WeKnora-agent-eval-postgres-dev",
        "createdb", "-U", $dbUser, "--template=template0", $dbName
    )
    Invoke-Docker -DockerArgs @(
        "exec", "-e", "PGPASSWORD=$dbPassword", "WeKnora-agent-eval-postgres-dev",
        "pg_restore", "-U", $dbUser, "-d", $dbName, "-j", [string]$PostgresJobs,
        "--no-owner", "/tmp/agent-eval-seed-db"
    )
    Invoke-Docker -DockerArgs @("exec", "WeKnora-agent-eval-postgres-dev", "rm", "-rf", "/tmp/agent-eval-seed-db")

    if (-not $SkipRedis) {
        Invoke-Docker -DockerArgs ((Eval-InfraPrefix) + @("stop", "redis"))
        $evalRedisVolume = "weknora-agent-eval-infra_redis_data_dev"
        $redisLoader = "weknora-agent-eval-redis-seed-loader"
        Invoke-Docker -DockerArgs @("rm", "-f", $redisLoader) -AllowFailure
        Invoke-Docker -DockerArgs @(
            "run", "--rm",
            "-v", "${evalRedisVolume}:/data",
            "-v", "${snapshotRoot}:/backup:ro",
            "--entrypoint", "/bin/sh", "redis:7.0-alpine", "-ec",
            "rm -rf /data/appendonlydir; cp /backup/redis.rdb /data/dump.rdb; chown redis:redis /data/dump.rdb"
        )

        # Starting the normal service directly with appendonly=yes would create
        # an empty AOF and hide the restored RDB. Load the RDB once without AOF,
        # enable AOF at runtime, wait for its initial rewrite, then hand the
        # volume back to the normal service.
        try {
            Invoke-Docker -DockerArgs @(
                "run", "-d", "--name", $redisLoader,
                "-e", "REDIS_PASSWORD=$redisPassword",
                "-v", "${evalRedisVolume}:/data",
                "--entrypoint", "/bin/sh", "redis:7.0-alpine", "-ec",
                'exec redis-server --dir /data --dbfilename dump.rdb --appendonly no --requirepass "$REDIS_PASSWORD"'
            )
            $redisReady = $false
            foreach ($attempt in 1..30) {
                & docker exec -e "REDISCLI_AUTH=$redisPassword" $redisLoader redis-cli --no-auth-warning ping *> $null
                if ($LASTEXITCODE -eq 0) { $redisReady = $true; break }
                Start-Sleep -Milliseconds 500
            }
            if (-not $redisReady) { throw "Redis seed loader did not become ready" }

            $sourceKeyspace = & docker exec -e "REDISCLI_AUTH=$redisPassword" $redisLoader redis-cli --no-auth-warning info keyspace
            Invoke-Docker -DockerArgs @(
                "exec", "-e", "REDISCLI_AUTH=$redisPassword", $redisLoader,
                "redis-cli", "--no-auth-warning", "config", "set", "appendonly", "yes"
            )
            $aofReady = $false
            foreach ($attempt in 1..120) {
                $persistence = & docker exec -e "REDISCLI_AUTH=$redisPassword" $redisLoader redis-cli --no-auth-warning info persistence
                if ($LASTEXITCODE -eq 0 -and
                    $persistence -match "aof_enabled:1" -and
                    $persistence -match "aof_rewrite_in_progress:0" -and
                    $persistence -match "aof_rewrite_scheduled:0") {
                    $aofReady = $true
                    break
                }
                Start-Sleep -Milliseconds 500
            }
            if (-not $aofReady) { throw "Redis initial AOF rewrite did not finish" }
            Invoke-Docker -DockerArgs @(
                "exec", "-e", "REDISCLI_AUTH=$redisPassword", $redisLoader,
                "redis-cli", "--no-auth-warning", "shutdown", "nosave"
            ) -AllowFailure
        } finally {
            Invoke-Docker -DockerArgs @("rm", "-f", $redisLoader) -AllowFailure
        }
        Invoke-Docker -DockerArgs ((Eval-InfraPrefix) + @("up", "-d", "--wait", "redis"))
        $targetKeyspace = & docker exec -e "REDISCLI_AUTH=$redisPassword" "WeKnora-agent-eval-redis-dev" redis-cli --no-auth-warning info keyspace
        if ($LASTEXITCODE -ne 0) { throw "failed to verify restored Redis keyspace" }
        $sourceKeyCount = [int64]0
        $targetKeyCount = [int64]0
        foreach ($line in $sourceKeyspace) {
            if ($line -match '^db\d+:keys=(\d+)') { $sourceKeyCount += [int64]$Matches[1] }
        }
        foreach ($line in $targetKeyspace) {
            if ($line -match '^db\d+:keys=(\d+)') { $targetKeyCount += [int64]$Matches[1] }
        }
        if ($targetKeyCount -ne $sourceKeyCount) {
            throw "Redis key-count mismatch after AOF conversion: source=$sourceKeyCount target=$targetKeyCount"
        }
    }

    if (-not $SkipNeo4j) {
        Invoke-Docker -DockerArgs @(
            "exec", "WeKnora-agent-eval-neo4j-dev", "cypher-shell", "-u", $neo4jUser, "-p", $neo4jPassword,
            "MATCH (n) DETACH DELETE n;"
        )
        Invoke-Docker -DockerArgs @("cp", (Join-Path $snapshotRoot "neo4j.cypher"), "WeKnora-agent-eval-neo4j-dev:/var/lib/neo4j/import/agent-eval-seed.cypher")
        Invoke-Docker -DockerArgs @(
            "exec", "WeKnora-agent-eval-neo4j-dev", "cypher-shell", "-u", $neo4jUser, "-p", $neo4jPassword,
            "-f", "/var/lib/neo4j/import/agent-eval-seed.cypher"
        )
        Invoke-Docker -DockerArgs @("exec", "WeKnora-agent-eval-neo4j-dev", "rm", "-f", "/var/lib/neo4j/import/agent-eval-seed.cypher")
    }

    foreach ($prefix in $minioPrefixes) {
        Restore-MinioPrefix "WeKnora-agent-eval-minio-dev" $evalNetwork $minioAccessKey $minioSecretKey $minioBucket $prefix $snapshotRoot
    }

    New-Item -ItemType Directory -Path $artifactDir -Force | Out-Null
    $hashes = [ordered]@{}
    foreach ($file in Get-ChildItem -LiteralPath $snapshotRoot -File -Recurse) {
        $relative = [System.IO.Path]::GetRelativePath($snapshotRoot, $file.FullName)
        $hashes[$relative] = (Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
    }
    $manifest = [ordered]@{
        schema_version = 1
        created_at = (Get-Date).ToUniversalTime().ToString("o")
        source_worktree = $mainRoot
        target_worktree = $repoRoot
        source_sizes_bytes = $sourceSizes
        estimated_source_bytes = $estimatedBytes
        full_minio = [bool]$FullMinio
        minio_bucket = $minioBucket
        minio_prefixes = $minioPrefixes
        redis_copied = -not [bool]$SkipRedis
        neo4j_copied = -not [bool]$SkipNeo4j
        langfuse = "fresh-v4-volume; old-v3 traces intentionally not physically copied"
        snapshot_sha256 = $hashes
    }
    $manifestPath = Join-Path $artifactDir "seed-manifest-$timestamp.json"
    $manifest | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $manifestPath -Encoding utf8
    $seedSucceeded = $true
    Write-Host "Seed completed. Manifest: $manifestPath"
} finally {
    if ($seedSucceeded -and (Test-Path -LiteralPath $snapshotRoot) -and -not $KeepSnapshot) {
        Remove-VerifiedSnapshot $snapshotRoot
    } elseif (Test-Path -LiteralPath $snapshotRoot) {
        Write-Host "Snapshot retained at $snapshotRoot (requested or seed incomplete)"
    }
}
