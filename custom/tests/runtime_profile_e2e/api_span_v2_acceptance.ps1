param(
    [string]$BaseUrl = "http://localhost:8080",
    [string]$AdminUsername = "37430534@qq.com",
    [ValidateRange(4, 24)][int]$DocumentCount = 12
)

$ErrorActionPreference = "Stop"
$token = ""
$knowledgeBaseId = ""
$originalPasswordHash = ""
$tempDirectory = ""
$knowledgeIds = @()
$runId = Get-Date -Format "yyyyMMddHHmmss"
$startedAt = [DateTime]::UtcNow

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw "Assertion failed: $Message" }
}

function Invoke-Db([string]$Sql) {
    $output = @(& docker exec WeKnora-postgres-dev psql -U postgres -d WeKnora -At -c $Sql)
    if ($LASTEXITCODE -ne 0) { throw "database query failed" }
    return @($output | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
}

function Invoke-DbScalar([string]$Sql) {
    return [string](@(Invoke-Db $Sql)[0])
}

function Invoke-Api {
    param(
        [string]$Method,
        [string]$Path,
        [object]$Body = $null,
        [int[]]$ExpectedStatus = @(200),
        [switch]$Anonymous
    )
    $arguments = @{
        Uri = "$BaseUrl$Path"
        Method = $Method
        Headers = @{}
        SkipHttpErrorCheck = $true
    }
    if (-not $Anonymous -and $token) {
        $arguments.Headers.Authorization = "Bearer $token"
    }
    if ($null -ne $Body) {
        $arguments.ContentType = "application/json"
        $arguments.Body = $Body | ConvertTo-Json -Depth 16 -Compress
    }
    $response = Invoke-WebRequest @arguments
    if ($response.StatusCode -notin $ExpectedStatus) {
        throw "$Method $Path returned $($response.StatusCode): $($response.Content)"
    }
    $json = $null
    if (-not [string]::IsNullOrWhiteSpace($response.Content)) {
        try { $json = $response.Content | ConvertFrom-Json } catch { $json = $null }
    }
    return [pscustomobject]@{ Status = [int]$response.StatusCode; Json = $json }
}

function Login-LocalAdmin {
    $safeUsername = $AdminUsername.Replace("'", "''")
    $script:originalPasswordHash = Invoke-DbScalar "
        SELECT password_hash FROM users
        WHERE username = '$safeUsername' AND deleted_at IS NULL AND is_system_admin = true
        LIMIT 1;
    "
    Assert-True (-not [string]::IsNullOrWhiteSpace($script:originalPasswordHash)) "admin hash exists"

    $password = "LocalSpanV2!" + [Guid]::NewGuid().ToString("N").Substring(0, 18)
    $hash = & python -c "import bcrypt,sys; print(bcrypt.hashpw(sys.argv[1].encode(), bcrypt.gensalt()).decode())" $password
    Assert-True ($LASTEXITCODE -eq 0) "temporary bcrypt hash generated"
    $updated = Invoke-Db "
        UPDATE users SET password_hash = '$hash', updated_at = NOW()
        WHERE username = '$safeUsername' AND deleted_at IS NULL AND is_system_admin = true
        RETURNING id;
    "
    Assert-True (
        (@($updated | Where-Object { $_ -match "^[0-9a-f-]{36}$" })).Count -eq 1
    ) "temporary local password installed"

    $challenge = (Invoke-Api -Method Get -Path "/api/v1/custom/auth-security/challenge" -Anonymous).Json.data
    $svg = [Text.Encoding]::UTF8.GetString(
        [Convert]::FromBase64String(($challenge.captcha_image -split ",", 2)[1])
    )
    $captcha = [regex]::Match($svg, "<text[^>]*>([^<]+)</text>").Groups[1].Value
    Assert-True ($captcha.Length -gt 0) "local SVG captcha parsed"
    $rsa = [Security.Cryptography.RSA]::Create()
    try {
        $rsa.ImportFromPem([string]$challenge.public_key)
        $encrypted = [Convert]::ToBase64String($rsa.Encrypt(
            [Text.Encoding]::UTF8.GetBytes($password),
            [Security.Cryptography.RSAEncryptionPadding]::OaepSHA256
        ))
    }
    finally { $rsa.Dispose() }
    $password = $null

    $login = Invoke-Api -Method Post -Path "/api/v1/auth/login" -Anonymous -Body @{
        username = $AdminUsername
        encrypted_password = $encrypted
        challenge_id = $challenge.challenge_id
        captcha_answer = $captcha
    }
    Assert-True ($login.Json.success -eq $true) "encrypted API login succeeds"
    $script:token = [string]$login.Json.token
    Assert-True (-not [string]::IsNullOrWhiteSpace($script:token)) "bearer token returned"
}

try {
    1..12 | ForEach-Object {
        $health = Invoke-Api -Method Get -Path "/health" -Anonymous
        Assert-True ($health.Json.status -eq "ok") "load-balanced health probe $_"
    }
    Login-LocalAdmin

    $legacyTableBefore = Invoke-DbScalar "SELECT COALESCE(to_regclass('public.knowledge_processing_spans')::text, '');"
    if ($legacyTableBefore) {
        $oldBefore = Invoke-DbScalar "
            SELECT count(*)::text || '|' || COALESCE(MAX(updated_at)::text, '')
            FROM public.knowledge_processing_spans;
        "
    }
    else {
        $oldBefore = "absent"
    }
    $models = (Invoke-Api -Method Get -Path "/api/v1/models").Json.data
    $embedding = $models | Where-Object { $_.type -eq "Embedding" } | Select-Object -First 1
    Assert-True ($null -ne $embedding) "embedding model available"
    $kb = Invoke-Api -Method Post -Path "/api/v1/knowledge-bases" -ExpectedStatus 201 -Body @{
        name = "Runtime-span-v2-$runId"
        type = "document"
        embedding_model_id = $embedding.id
        storage_provider_config = @{ provider = "local" }
        question_generation_config = @{ enabled = $true; question_count = 1 }
        graph_enabled = $false
    }
    $knowledgeBaseId = [string]$kb.Json.data.id
    Assert-True ($knowledgeBaseId -match "^[0-9a-f-]{36}$") "knowledge base created"

    $tempDirectory = Join-Path ([IO.Path]::GetTempPath()) "weknora-span-v2-$runId"
    [IO.Directory]::CreateDirectory($tempDirectory) | Out-Null
    $paths = 1..$DocumentCount | ForEach-Object {
        $path = Join-Path $tempDirectory ("span-v2-{0:D2}.md" -f $_)
        [IO.File]::WriteAllText(
            $path,
            "# Span V2 concurrent probe $_`n`nSPV2-$runId-$_ verifies provider admission, stable logical spans, retrieval indexing, and durable derivative processing under a small concurrent batch.",
            [Text.UTF8Encoding]::new($false)
        )
        $path
    }

    $uploadResults = @($paths | ForEach-Object -Parallel {
        $response = Invoke-WebRequest `
            -Uri "$using:BaseUrl/api/v1/knowledge-bases/$using:knowledgeBaseId/knowledge/file" `
            -Method Post `
            -Headers @{ Authorization = "Bearer $using:token" } `
            -Form @{ file = Get-Item -LiteralPath $_; fileName = Split-Path -Leaf $_ } `
            -SkipHttpErrorCheck
        if ($response.StatusCode -notin @(200, 201)) {
            throw "upload failed with $($response.StatusCode): $($response.Content)"
        }
        ($response.Content | ConvertFrom-Json).data.id
    } -ThrottleLimit $DocumentCount)
    $knowledgeIds = @($uploadResults | Where-Object { $_ -match "^[0-9a-f-]{36}$" })
    Assert-True ($knowledgeIds.Count -eq $DocumentCount) "$DocumentCount concurrent API uploads accepted"
    $idSql = ($knowledgeIds | ForEach-Object { "'$_'" }) -join ","

    # Require an observed control-plane wait, then prove those work items have
    # no logical leaf span until provider_attempts crosses the real-call boundary.
    $waitDeadline = [DateTime]::UtcNow.AddMinutes(3)
    $waiting = 0
    do {
        $waiting = [int](Invoke-DbScalar "
            SELECT count(*) FROM custom_derivative_work_items
            WHERE knowledge_id IN ($idSql)
              AND work_kind IN ('summary','question_batch')
              AND provider_attempts = 0
              AND state NOT IN ('completed','failed','provider_unknown','cancelled');
        ")
        if ($waiting -gt 0) { break }
        Start-Sleep -Seconds 1
    } while ([DateTime]::UtcNow -lt $waitDeadline)
    Assert-True ($waiting -gt 0) "concurrent batch produced an observable provider-capacity wait"

    $premature = [int](Invoke-DbScalar "
        WITH waiting AS (
            SELECT knowledge_id, processing_attempt,
                   CASE
                       WHEN work_kind = 'summary' THEN 'derivative:summary'
                       WHEN work_kind = 'question_batch' THEN
                           'derivative:question_batch:' || substring(item_id from '\[([0-9]+)\]')
                   END AS logical_key
            FROM custom_derivative_work_items
            WHERE knowledge_id IN ($idSql)
              AND work_kind IN ('summary','question_batch')
              AND provider_attempts = 0
              AND state NOT IN ('completed','failed','provider_unknown','cancelled')
        )
        SELECT count(*) FROM waiting
        JOIN custom_processing_spans_v2 AS spans
          ON spans.knowledge_id = waiting.knowledge_id
         AND spans.attempt = waiting.processing_attempt
         AND spans.logical_key = waiting.logical_key;
    ")
    Assert-True ($premature -eq 0) "capacity waits create zero business-span rows"

    $terminalDeadline = [DateTime]::UtcNow.AddMinutes(10)
    do {
        $completed = [int](Invoke-DbScalar "
            SELECT count(*) FROM knowledges
            WHERE id IN ($idSql) AND parse_status = 'completed';
        ")
        $failed = [int](Invoke-DbScalar "
            SELECT count(*) FROM knowledges
            WHERE id IN ($idSql) AND parse_status IN ('failed','cancelled');
        ")
        if (($completed + $failed) -eq $DocumentCount) { break }
        Start-Sleep -Seconds 3
    } while ([DateTime]::UtcNow -lt $terminalDeadline)
    Assert-True ($failed -eq 0) "no probe document failed"
    Assert-True ($completed -eq $DocumentCount) "all probe documents completed"

    $duplicates = [int](Invoke-DbScalar "
        SELECT count(*) FROM (
            SELECT knowledge_id, attempt, logical_key
            FROM custom_processing_spans_v2
            WHERE knowledge_id IN ($idSql)
            GROUP BY knowledge_id, attempt, logical_key
            HAVING count(*) > 1
        ) AS duplicate_keys;
    ")
    Assert-True ($duplicates -eq 0) "one V2 row per logical key"
    $controlPlaneFailures = [int](Invoke-DbScalar "
        SELECT count(*) FROM custom_processing_spans_v2
        WHERE knowledge_id IN ($idSql)
          AND (last_error_code ILIKE '%ADMISSION%'
            OR last_error_code ILIKE '%CAPACITY%'
            OR last_error_code ILIKE '%CIRCUIT%');
    ")
    Assert-True ($controlPlaneFailures -eq 0) "control-plane waits are not business failures"

    foreach ($knowledgeId in $knowledgeIds) {
        $spans = Invoke-Api -Method Get -Path "/api/v1/knowledge/$knowledgeId/spans"
        Assert-True ($spans.Json.success -eq $true) "V2 span API readable for $knowledgeId"
    }
    $legacyTableAfter = Invoke-DbScalar "SELECT COALESCE(to_regclass('public.knowledge_processing_spans')::text, '');"
    if ($legacyTableBefore) {
        $oldAfter = Invoke-DbScalar "
            SELECT count(*)::text || '|' || COALESCE(MAX(updated_at)::text, '')
            FROM public.knowledge_processing_spans;
        "
        Assert-True ($oldAfter -eq $oldBefore) "legacy table received zero writes during API concurrency"
    }
    else {
        $oldAfter = "absent"
        Assert-True (-not $legacyTableAfter) "legacy table remains absent"
    }

    Write-Host "[PASS] health=12 uploads=$DocumentCount observed_waiting=$waiting premature_spans=$premature"
    Write-Host "[PASS] completed=$completed duplicate_logical_keys=$duplicates control_plane_failures=$controlPlaneFailures"
    Write-Host "[PASS] legacy_table=$oldAfter"
}
finally {
    if ($knowledgeBaseId -and $token) {
        try {
            Invoke-Api -Method Delete -Path "/api/v1/knowledge-bases/$knowledgeBaseId" `
                -ExpectedStatus @(200, 202, 404) | Out-Null
        }
        catch { Write-Warning "failed to remove span V2 acceptance knowledge base" }
    }
    if ($originalPasswordHash) {
        $safeUsername = $AdminUsername.Replace("'", "''")
        $safeHash = $originalPasswordHash.Replace("'", "''")
        try {
            Invoke-Db "
                UPDATE users SET password_hash = '$safeHash', updated_at = NOW()
                WHERE username = '$safeUsername' AND deleted_at IS NULL;
            " | Out-Null
        }
        catch { Write-Warning "failed to restore local administrator password hash" }
    }
    if ($tempDirectory -and (Test-Path -LiteralPath $tempDirectory)) {
        Remove-Item -LiteralPath $tempDirectory -Recurse -Force
    }
}
