param(
    [string]$BaseUrl = "http://localhost:8080",
    [string]$AdminUsername = "37430534@qq.com"
)

$ErrorActionPreference = "Stop"
$script:Token = ""
$script:CreatedKnowledgeBases = [System.Collections.Generic.List[string]]::new()
$script:CreatedPoolId = ""
$script:CreatedPoolVersion = 0
$script:Passed = [System.Collections.Generic.List[string]]::new()
$runId = Get-Date -Format "yyyyMMddHHmmss"

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
        [switch]$Anonymous,
        [hashtable]$ExtraHeaders = @{}
    )
    $headers = @{}
    if (-not $Anonymous -and $script:Token) {
        $headers.Authorization = "Bearer $($script:Token)"
    }
    foreach ($entry in $ExtraHeaders.GetEnumerator()) {
        if ($entry.Key -ieq "If-Match") {
            $headers[$entry.Key] = '"' + [string]$entry.Value + '"'
        }
        else {
            $headers[$entry.Key] = $entry.Value
        }
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

function Set-LocalAdminPassword {
    $password = "LocalStage1!" + [Guid]::NewGuid().ToString("N").Substring(0, 16)
    $hash = & python -c "import bcrypt,sys; print(bcrypt.hashpw(sys.argv[1].encode(), bcrypt.gensalt()).decode())" $password
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($hash)) {
        throw "failed to create a local bcrypt password hash"
    }
    # bcrypt output uses a fixed alphabet that excludes SQL quotes. Escape
    # the configurable local username before embedding it in the one-shot SQL.
    $safeUsername = $AdminUsername.Replace("'", "''")
    $output = & docker exec WeKnora-postgres-dev psql -U postgres -d WeKnora -At `
        -c "UPDATE users SET password_hash = '$hash', updated_at = NOW() WHERE username = '$safeUsername' AND is_system_admin = true RETURNING id;"
    if ($LASTEXITCODE -ne 0 -or ($output | Where-Object { $_ -match "^[0-9a-f-]{36}$" }).Count -ne 1) {
        throw "failed to reset the local-only system administrator password"
    }
    return $password
}

function Login {
    param([string]$Password)
    $challenge = Get-AuthChallenge
    $encrypted = Protect-Password -PlainText $Password -PublicKey $challenge.PublicKey
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

function New-DocumentKnowledgeBase {
    param([object]$QuestionConfig = $null)
    $body = @{
        name = "Runtime-stage1-$runId-$($script:CreatedKnowledgeBases.Count + 1)"
        type = "document"
        storage_provider_config = @{ provider = "local" }
    }
    if ($null -ne $QuestionConfig) {
        $body.question_generation_config = $QuestionConfig
    }
    $response = Invoke-Api -Method Post -Path "/api/v1/knowledge-bases" -Body $body -ExpectedStatus 201
    $script:CreatedKnowledgeBases.Add([string]$response.Json.data.id)
    return $response.Json.data
}

try {
    $health = Invoke-Api -Method Get -Path "/lb-health" -Anonymous
    Assert-True ($health.Json.topology -eq "runtime-profile-e2e") "requests enter through the local multi-instance load balancer"
    1..12 | ForEach-Object {
        $probe = Invoke-Api -Method Get -Path "/health" -Anonymous
        Assert-True ($probe.Json.status -eq "ok") "API health probe $_ succeeds"
    }
    Pass "three-replica API entry and repeated health probes"

    $password = Set-LocalAdminPassword
    Login -Password $password
    $password = $null
    Pass "local captcha, RSA password transport, and system-admin authentication"

    $pools = Invoke-Api -Method Get -Path "/api/v1/custom/derivative-control/resource-pools"
    $bindings = Invoke-Api -Method Get -Path "/api/v1/custom/derivative-control/bindings"
    $templates = Invoke-Api -Method Get -Path "/api/v1/custom/derivative-control/templates"
    Assert-True ($pools.Json.data.Count -ge 1) "resource pools are reconciled"
    Assert-True ($bindings.Json.data.Count -ge 1) "model bindings are reconciled"
    Assert-True ($templates.Json.data.Count -ge 1) "admission templates are seeded"
    Assert-True (($bindings.Json.data | Where-Object { [string]::IsNullOrWhiteSpace($_.route_fingerprint) }).Count -eq 0) "every binding has an actual-route fingerprint"
    Pass "model resource pools, exact-route bindings, and templates"

    $poolId = "stage1-$runId"
    $poolBody = @{
        id = $poolId
        name = "Stage 1 acceptance pool"
        resource_kind = "derivative"
        max_inflight = 4
        max_background_inflight = 3
        interactive_reserve = 1
        tenant_guaranteed = 1
        tenant_burst = 2
        document_guaranteed = 1
        document_burst = 2
        rpm = 60
        tpm = 100000
        token_burst = 20000
        request_timeout_seconds = 900
        circuit_threshold = 3
        circuit_window_seconds = 600
        circuit_open_seconds = 60
        state = "enabled"
    }
    $created = Invoke-Api -Method Post -Path "/api/v1/custom/derivative-control/resource-pools" -Body $poolBody -ExpectedStatus 201
    $script:CreatedPoolId = $poolId
    $script:CreatedPoolVersion = [int]$created.Json.data.policy_version
    Assert-True ($script:CreatedPoolVersion -eq 1) "new resource pool starts at policy version 1"

    $poolBody.max_inflight = 5
    $poolBody.max_background_inflight = 4
    $updated = Invoke-Api -Method Put -Path "/api/v1/custom/derivative-control/resource-pools/$poolId" `
        -Body $poolBody -ExtraHeaders @{ "If-Match" = "1" }
    Assert-True ($updated.Json.data.policy_version -eq 2) "resource pool update advances policy version"
    $script:CreatedPoolVersion = 2

    Invoke-Api -Method Put -Path "/api/v1/custom/derivative-control/resource-pools/$poolId" `
        -Body $poolBody -ExtraHeaders @{ "If-Match" = "1" } -ExpectedStatus 409 | Out-Null
    $drained = Invoke-Api -Method Post -Path "/api/v1/custom/derivative-control/resource-pools/$poolId/drain" `
        -ExtraHeaders @{ "If-Match" = "2" }
    Assert-True ($drained.Json.success -eq $true) "resource pool enters draining state"
    $script:CreatedPoolVersion = 3
    Invoke-Api -Method Delete -Path "/api/v1/custom/derivative-control/resource-pools/$poolId" `
        -ExtraHeaders @{ "If-Match" = "3" } | Out-Null
    $script:CreatedPoolId = ""
    $audits = Invoke-Api -Method Get -Path "/api/v1/custom/derivative-control/audits"
    Assert-True (($audits.Json.data | Where-Object { $_.resource_id -eq $poolId -and $_.action -eq "delete" }).Count -eq 1) "pool deletion is audited"
    Pass "resource pool create, hot update, optimistic conflict, drain, delete, and audit"

    $queue = Invoke-Api -Method Get -Path "/api/v1/custom/derivative-control/queue/status"
    Assert-True ($queue.Json.data.PSObject.Properties.Name -contains "work_items") "queue status exposes durable work item aggregation"
    Assert-True ($queue.Json.data.resource_pools -ge 1) "queue status exposes resource pool count"
    Pass "durable queue and admission status API"

    $defaultKB = New-DocumentKnowledgeBase
    Assert-True ($defaultKB.question_generation_config.enabled -eq $true) "question generation defaults to enabled"
    Assert-True ($defaultKB.question_generation_config.question_count -eq 1) "default question count is one"
    $cappedKB = New-DocumentKnowledgeBase -QuestionConfig @{ enabled = $true; question_count = 99 }
    Assert-True ($cappedKB.question_generation_config.enabled -eq $true) "explicit question generation stays enabled"
    Assert-True ($cappedKB.question_generation_config.question_count -eq 3) "question count is capped at three"
    Pass "question generation default-on, default one, and hard cap three"

    foreach ($kbId in @($script:CreatedKnowledgeBases)) {
        Invoke-Api -Method Delete -Path "/api/v1/knowledge-bases/$kbId" | Out-Null
        [void]$script:CreatedKnowledgeBases.Remove($kbId)
    }

    Write-Host ""
    Write-Host "Stage 1 API acceptance passed: $($script:Passed.Count) groups"
}
finally {
    foreach ($kbId in @($script:CreatedKnowledgeBases)) {
        try {
            Invoke-Api -Method Delete -Path "/api/v1/knowledge-bases/$kbId" -ExpectedStatus @(200, 202, 404) | Out-Null
        }
        catch {
            Write-Warning "failed to clean up knowledge base $kbId"
        }
    }
    if (-not [string]::IsNullOrWhiteSpace($script:CreatedPoolId)) {
        try {
            if ($script:CreatedPoolVersion -lt 3) {
                Invoke-Api -Method Post -Path "/api/v1/custom/derivative-control/resource-pools/$($script:CreatedPoolId)/drain" `
                    -ExtraHeaders @{ "If-Match" = "$($script:CreatedPoolVersion)" } | Out-Null
                $script:CreatedPoolVersion++
            }
            Invoke-Api -Method Delete -Path "/api/v1/custom/derivative-control/resource-pools/$($script:CreatedPoolId)" `
                -ExtraHeaders @{ "If-Match" = "$($script:CreatedPoolVersion)" } -ExpectedStatus @(200, 404) | Out-Null
        }
        catch {
            Write-Warning "failed to clean up resource pool $($script:CreatedPoolId)"
        }
    }
}
