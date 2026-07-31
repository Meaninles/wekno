param(
    [string]$BaseUrl = "http://localhost:8080",
    [string]$AdminUsername = "37430534@qq.com"
)

$ErrorActionPreference = "Stop"
$script:Token = ""
$script:KnowledgeBaseId = ""
$script:KnowledgeId = ""
$script:StoppedMaintenance = ""
$script:DerivativeWorkersStopped = $false
$script:Passed = [System.Collections.Generic.List[string]]::new()
$runId = Get-Date -Format "yyyyMMddHHmmss"
$marker = "DURABLE-CORE-SEARCH-$runId"
$derivativeWorkers = @(
    "weknora-runtime-derivative-1",
    "weknora-runtime-derivative-2"
)

function Assert-True {
    param([bool]$Condition, [string]$Message)
    if (-not $Condition) {
        throw "Assertion failed: $Message"
    }
}

function Pass {
    param([string]$Name)
    $script:Passed.Add($Name)
    Write-Host "[PASS] $Name"
}

function Invoke-Api {
    param(
        [string]$Method,
        [string]$Path,
        [object]$Body = $null,
        [int[]]$ExpectedStatus = @(200),
        [switch]$Anonymous
    )
    $headers = @{}
    if (-not $Anonymous -and $script:Token) {
        $headers.Authorization = "Bearer $($script:Token)"
    }
    $arguments = @{
        Uri = "$BaseUrl$Path"
        Method = $Method
        Headers = $headers
        SkipHttpErrorCheck = $true
    }
    if ($null -ne $Body) {
        $arguments.ContentType = "application/json"
        $arguments.Body = $Body | ConvertTo-Json -Depth 16 -Compress
    }
    $response = Invoke-WebRequest @arguments
    if ($response.StatusCode -notin $ExpectedStatus) {
        throw "$Method $Path returned $($response.StatusCode), expected $($ExpectedStatus -join ','): $($response.Content)"
    }
    $json = $null
    if (-not [string]::IsNullOrWhiteSpace($response.Content)) {
        try {
            $json = $response.Content | ConvertFrom-Json
        }
        catch {
            $json = $null
        }
    }
    return [pscustomobject]@{
        Status = [int]$response.StatusCode
        Json = $json
        Content = $response.Content
    }
}

function Get-AuthChallenge {
    $response = Invoke-Api -Method Get -Path "/api/v1/custom/auth-security/challenge" -Anonymous
    $encoded = ($response.Json.data.captcha_image -split ",", 2)[1]
    $svg = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encoded))
    $answer = [regex]::Match($svg, "<text[^>]*>([^<]+)</text>").Groups[1].Value
    Assert-True ($answer.Length -gt 0) "captcha answer is present in the local SVG challenge"
    return @{
        Id = $response.Json.data.challenge_id
        PublicKey = $response.Json.data.public_key
        Answer = $answer
    }
}

function Protect-Password {
    param([string]$PlainText, [string]$PublicKey)
    $rsa = [Security.Cryptography.RSA]::Create()
    try {
        $rsa.ImportFromPem($PublicKey)
        $cipher = $rsa.Encrypt(
            [Text.Encoding]::UTF8.GetBytes($PlainText),
            [Security.Cryptography.RSAEncryptionPadding]::OaepSHA256
        )
        return [Convert]::ToBase64String($cipher)
    }
    finally {
        $rsa.Dispose()
    }
}

function Login-LocalAdmin {
    $password = "LocalStage2!" + [Guid]::NewGuid().ToString("N").Substring(0, 16)
    $hash = & python -c "import bcrypt,sys; print(bcrypt.hashpw(sys.argv[1].encode(), bcrypt.gensalt()).decode())" $password
    Assert-True ($LASTEXITCODE -eq 0 -and -not [string]::IsNullOrWhiteSpace($hash)) "local bcrypt hash was generated"
    $safeUsername = $AdminUsername.Replace("'", "''")
    $updated = & docker exec WeKnora-postgres-dev psql -U postgres -d WeKnora -At `
        -c "UPDATE users SET password_hash = '$hash', updated_at = NOW() WHERE username = '$safeUsername' AND is_system_admin = true RETURNING id;"
    Assert-True (
        $LASTEXITCODE -eq 0 -and
        ($updated | Where-Object { $_ -match "^[0-9a-f-]{36}$" }).Count -eq 1
    ) "local-only system administrator password was reset"

    $challenge = Get-AuthChallenge
    $encrypted = Protect-Password -PlainText $password -PublicKey $challenge.PublicKey
    $password = $null
    $response = Invoke-Api -Method Post -Path "/api/v1/auth/login" -Anonymous -Body @{
        username = $AdminUsername
        encrypted_password = $encrypted
        challenge_id = $challenge.Id
        captcha_answer = $challenge.Answer
    }
    Assert-True ($response.Json.success -eq $true) "encrypted administrator login succeeds"
    Assert-True (-not [string]::IsNullOrWhiteSpace($response.Json.token)) "login returns a bearer token"
    $script:Token = $response.Json.token
}

function Invoke-DbScalar {
    param([string]$Sql)
    $output = & docker exec WeKnora-postgres-dev psql -U postgres -d WeKnora -At -c $Sql
    if ($LASTEXITCODE -ne 0) {
        throw "database query failed"
    }
    return [string](@($output | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })[0])
}

function Remove-StaleStage2KnowledgeBases {
    $ids = @(& docker exec WeKnora-postgres-dev psql -U postgres -d WeKnora -At -c "
        SELECT id FROM knowledge_bases
        WHERE name LIKE 'Runtime-stage2-%' AND deleted_at IS NULL
        ORDER BY created_at;
    " | Where-Object { $_ -match "^[0-9a-f-]{36}$" })
    foreach ($id in $ids) {
        Invoke-Api -Method Delete -Path "/api/v1/knowledge-bases/$id" `
            -ExpectedStatus @(200, 202, 404) | Out-Null
    }
}

function Wait-ContainerHealthy {
    param([string[]]$Names, [int]$TimeoutSeconds = 60)
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $unhealthy = @()
        foreach ($name in $Names) {
            $state = & docker inspect $name --format "{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}"
            if ($state -ne "healthy" -and $state -ne "running") {
                $unhealthy += "$name=$state"
            }
        }
        if ($unhealthy.Count -eq 0) {
            return
        }
        Start-Sleep -Seconds 2
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "containers did not become healthy: $($unhealthy -join ', ')"
}

function Get-CurrentMaintenanceLeader {
    $events = @()
    foreach ($name in @("weknora-runtime-maintenance-1", "weknora-runtime-maintenance-2")) {
        $lines = @(& docker logs --timestamps --since 1h $name 2>&1 |
            Select-String -SimpleMatch "acquired PostgreSQL advisory lock")
        foreach ($line in $lines) {
            $timestampText = ([string]$line).Split(" ", 2)[0]
            $timestamp = [DateTimeOffset]::MinValue
            if ([DateTimeOffset]::TryParse($timestampText, [ref]$timestamp)) {
                $events += [pscustomobject]@{ Name = $name; Timestamp = $timestamp }
            }
        }
    }
    $latest = $events | Sort-Object Timestamp -Descending | Select-Object -First 1
    if ($null -eq $latest) {
        throw "no maintenance leader acquisition event was found"
    }
    return [string]$latest.Name
}

function Test-MaintenanceFailover {
    $leader = Get-CurrentMaintenanceLeader
    $standby = if ($leader -eq "weknora-runtime-maintenance-1") {
        "weknora-runtime-maintenance-2"
    }
    else {
        "weknora-runtime-maintenance-1"
    }
    $before = @(& docker logs --since 1h $standby 2>&1 |
        Select-String -SimpleMatch "acquired PostgreSQL advisory lock").Count
    & docker stop --time 30 $leader | Out-Null
    Assert-True ($LASTEXITCODE -eq 0) "current maintenance leader stopped locally"
    $script:StoppedMaintenance = $leader

    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    $healthyProbes = 0
    do {
        $health = Invoke-Api -Method Get -Path "/health" -Anonymous
        if ($health.Json.status -eq "ok") {
            $healthyProbes++
        }
        $after = @(& docker logs --since 1h $standby 2>&1 |
            Select-String -SimpleMatch "acquired PostgreSQL advisory lock").Count
        if ($after -gt $before) {
            break
        }
        Start-Sleep -Seconds 2
    } while ([DateTime]::UtcNow -lt $deadline)
    Assert-True ($after -gt $before) "standby acquired maintenance leadership within 30 seconds"
    Assert-True ($healthyProbes -ge 1) "API stayed available during maintenance failover"

    & docker start $leader | Out-Null
    Assert-True ($LASTEXITCODE -eq 0) "former leader restarted as standby"
    Wait-ContainerHealthy -Names @($leader)
    $script:StoppedMaintenance = ""
}

function Upload-ProbeDocument {
    param([string]$Path)
    $headers = @{ Authorization = "Bearer $($script:Token)" }
    $response = Invoke-WebRequest `
        -Uri "$BaseUrl/api/v1/knowledge-bases/$($script:KnowledgeBaseId)/knowledge/file" `
        -Method Post `
        -Headers $headers `
        -Form @{
            file = Get-Item -LiteralPath $Path
            fileName = Split-Path -Leaf $Path
        } `
        -SkipHttpErrorCheck
    if ($response.StatusCode -notin @(200, 201)) {
        throw "document upload returned $($response.StatusCode): $($response.Content)"
    }
    $json = $response.Content | ConvertFrom-Json
    Assert-True (-not [string]::IsNullOrWhiteSpace($json.data.id)) "upload returns a knowledge id"
    return [string]$json.data.id
}

try {
    1..9 | ForEach-Object {
        $health = Invoke-Api -Method Get -Path "/health" -Anonymous
        Assert-True ($health.Json.status -eq "ok") "load-balanced health probe $_ succeeds"
    }
    Login-LocalAdmin
    Remove-StaleStage2KnowledgeBases
    Test-MaintenanceFailover
    Pass "API continuity and PostgreSQL-session maintenance failover"

    $models = (Invoke-Api -Method Get -Path "/api/v1/models").Json.data
    $embedding = $models | Where-Object { $_.type -eq "Embedding" } | Select-Object -First 1
    Assert-True ($null -ne $embedding) "an embedding model is available locally"
    $createdKB = Invoke-Api -Method Post -Path "/api/v1/knowledge-bases" -ExpectedStatus 201 -Body @{
        name = "Runtime-stage2-$runId"
        type = "document"
        embedding_model_id = $embedding.id
        storage_provider_config = @{ provider = "local" }
        question_generation_config = @{ enabled = $true; question_count = 1 }
        graph_enabled = $false
    }
    $script:KnowledgeBaseId = [string]$createdKB.Json.data.id

    & docker stop --time 30 @derivativeWorkers | Out-Null
    Assert-True ($LASTEXITCODE -eq 0) "both local derivative workers stopped"
    $script:DerivativeWorkersStopped = $true

    $tempDirectory = Join-Path ([IO.Path]::GetTempPath()) "weknora-runtime-stage2-$runId"
    [IO.Directory]::CreateDirectory($tempDirectory) | Out-Null
    $probePath = Join-Path $tempDirectory "durable-core-$runId.md"
    [IO.File]::WriteAllText(
        $probePath,
        "# Durable local acceptance`n`n$marker proves the core index is searchable while model-backed enrichment is still pending.`n`nThe same marker is repeated for stable keyword recall: $marker.",
        [Text.UTF8Encoding]::new($false)
    )
    $script:KnowledgeId = Upload-ProbeDocument -Path $probePath
    Assert-True ($script:KnowledgeId -match "^[0-9a-f-]{36}$") "knowledge id is a UUID"

    $coreDeadline = [DateTime]::UtcNow.AddMinutes(5)
    do {
        $knowledge = (Invoke-Api -Method Get -Path "/api/v1/knowledge/$($script:KnowledgeId)").Json.data
        if ($knowledge.core_status -eq "ready") {
            break
        }
        if ($knowledge.parse_status -in @("failed", "cancelled")) {
            throw "core processing ended in $($knowledge.parse_status): $($knowledge.error_message)"
        }
        Start-Sleep -Seconds 2
    } while ([DateTime]::UtcNow -lt $coreDeadline)
    Assert-True ($knowledge.core_status -eq "ready") "core index becomes ready while derivative workers are stopped"
    Assert-True ($knowledge.parse_status -eq "finalizing") "document remains in finalizing during enrichment outage"

    $durableDeadline = [DateTime]::UtcNow.AddSeconds(60)
    do {
        $workItemCount = [int](Invoke-DbScalar -Sql "
            SELECT count(*) FROM custom_derivative_work_items
            WHERE knowledge_id = '$($script:KnowledgeId)'
              AND processing_generation = '$($knowledge.processing_generation)';
        ")
        if ($workItemCount -ge 2) {
            break
        }
        Start-Sleep -Seconds 1
    } while ([DateTime]::UtcNow -lt $durableDeadline)
    Assert-True ($workItemCount -ge 2) "PostgreSQL contains durable derivative work plus finalizer"
    $activeCount = [int](Invoke-DbScalar -Sql "
        SELECT count(*) FROM custom_derivative_work_items
        WHERE knowledge_id = '$($script:KnowledgeId)'
          AND state NOT IN ('completed','failed','provider_unknown','cancelled');
    ")
    Assert-True ($activeCount -ge 1) "derivative work remains recoverably non-terminal without workers"

    $hybrid = Invoke-Api -Method Post `
        -Path "/api/v1/knowledge-bases/$($script:KnowledgeBaseId)/hybrid-search" `
        -Body @{
            query_text = $marker
            match_count = 10
            knowledge_ids = @($script:KnowledgeId)
            vector_threshold = 0
            keyword_threshold = 0
            disable_vector_match = $true
        }
    Assert-True (
        (@($hybrid.Json.data | Where-Object { $_.knowledge_id -eq $script:KnowledgeId })).Count -ge 1
    ) "core-ready document is searchable before enrichment completes"
    Pass "durable PostgreSQL work and core-ready retrieval during full derivative outage"

    & docker restart WeKnora-redis-dev | Out-Null
    Assert-True ($LASTEXITCODE -eq 0) "local Redis restarted"
    $redisDeadline = [DateTime]::UtcNow.AddSeconds(60)
    do {
        $ping = & docker exec WeKnora-redis-dev sh -lc 'redis-cli -a "$REDIS_PASSWORD" ping' 2>$null
        if ($ping -match "PONG") {
            break
        }
        Start-Sleep -Seconds 2
    } while ([DateTime]::UtcNow -lt $redisDeadline)
    # The local image can source its password from the container rather than
    # the host environment, so API recovery is the authoritative assertion.
    $apiRecovered = $false
    $apiDeadline = [DateTime]::UtcNow.AddSeconds(60)
    do {
        try {
            $probe = Invoke-Api -Method Get -Path "/health" -Anonymous
            $apiRecovered = $probe.Json.status -eq "ok"
        }
        catch {
            $apiRecovered = $false
        }
        if ($apiRecovered) {
            break
        }
        Start-Sleep -Seconds 2
    } while ([DateTime]::UtcNow -lt $apiDeadline)
    Assert-True $apiRecovered "API recovers after the local Redis restart"

    & docker start @derivativeWorkers | Out-Null
    Assert-True ($LASTEXITCODE -eq 0) "derivative workers restarted"
    Wait-ContainerHealthy -Names $derivativeWorkers
    $script:DerivativeWorkersStopped = $false

    $terminalDeadline = [DateTime]::UtcNow.AddMinutes(6)
    do {
        $knowledge = (Invoke-Api -Method Get -Path "/api/v1/knowledge/$($script:KnowledgeId)").Json.data
        $activeCount = [int](Invoke-DbScalar -Sql "
            SELECT count(*) FROM custom_derivative_work_items
            WHERE knowledge_id = '$($script:KnowledgeId)'
              AND state NOT IN ('completed','failed','provider_unknown','cancelled');
        ")
        if ($activeCount -eq 0 -and $knowledge.parse_status -in @("completed", "failed")) {
            break
        }
        Start-Sleep -Seconds 3
    } while ([DateTime]::UtcNow -lt $terminalDeadline)
    Assert-True ($activeCount -eq 0) "all derivative work reaches a terminal PostgreSQL state"
    Assert-True ($knowledge.parse_status -eq "completed") "finalizer promotes the document to completed"
    Assert-True ($knowledge.enrichment_status -in @("completed", "degraded", "failed")) "enrichment outcome is explicit"

    $finalizerCompleted = [int](Invoke-DbScalar -Sql "
        SELECT count(*) FROM custom_derivative_work_items
        WHERE knowledge_id = '$($script:KnowledgeId)'
          AND work_kind = 'finalizer'
          AND state = 'completed';
    ")
    $providerCalls = [int](Invoke-DbScalar -Sql "
        SELECT count(*) FROM custom_derivative_provider_calls AS calls
        JOIN custom_derivative_work_items AS items ON items.id = calls.work_item_id
        WHERE items.knowledge_id = '$($script:KnowledgeId)';
    ")
    $resultRows = [int](Invoke-DbScalar -Sql "
        SELECT count(*) FROM custom_derivative_results AS results
        JOIN custom_derivative_work_items AS items ON items.id = results.work_item_id
        WHERE items.knowledge_id = '$($script:KnowledgeId)';
    ")
    $duplicateLogicalSpans = [int](Invoke-DbScalar -Sql "
        SELECT count(*) FROM (
            SELECT knowledge_id, attempt, logical_key
            FROM custom_processing_spans_v2
            WHERE knowledge_id = '$($script:KnowledgeId)'
            GROUP BY knowledge_id, attempt, logical_key
            HAVING count(*) > 1
        ) AS duplicates;
    ")
    Assert-True ($finalizerCompleted -eq 1) "exactly one durable finalizer completed"
    Assert-True ($providerCalls -ge 1) "actual model calls were checkpointed before materialization"
    Assert-True ($resultRows -ge 1) "immutable derivative results were persisted"
    Assert-True ($duplicateLogicalSpans -eq 0) "Span V2 contains one row per logical key"

    $spans = Invoke-Api -Method Get -Path "/api/v1/knowledge/$($script:KnowledgeId)/spans"
    Assert-True ($spans.Json.success -eq $true) "Span V2 API remains readable after retries and recovery"
    Pass "Redis recovery, durable result-first execution, finalizer settlement, and Span V2 uniqueness"

    Write-Host ""
    Write-Host "Stage 2 API/fault acceptance passed: $($script:Passed.Count) groups"
}
finally {
    if ($script:DerivativeWorkersStopped) {
        & docker start @derivativeWorkers | Out-Null
    }
    if (-not [string]::IsNullOrWhiteSpace($script:StoppedMaintenance)) {
        & docker start $script:StoppedMaintenance | Out-Null
    }
    if (-not [string]::IsNullOrWhiteSpace($script:KnowledgeBaseId)) {
        try {
            Invoke-Api -Method Delete -Path "/api/v1/knowledge-bases/$($script:KnowledgeBaseId)" `
                -ExpectedStatus @(200, 202, 404) | Out-Null
        }
        catch {
            Write-Warning "failed to clean up knowledge base $($script:KnowledgeBaseId)"
        }
    }
    if (Test-Path variable:tempDirectory) {
        $resolvedTempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
        $resolvedDirectory = [IO.Path]::GetFullPath($tempDirectory)
        if (
            $resolvedDirectory.StartsWith($resolvedTempRoot, [StringComparison]::OrdinalIgnoreCase) -and
            (Split-Path -Leaf $resolvedDirectory).StartsWith("weknora-runtime-stage2-")
        ) {
            Remove-Item -LiteralPath $resolvedDirectory -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}
