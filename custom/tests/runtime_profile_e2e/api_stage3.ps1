param(
    [string]$BaseUrl = "http://localhost:8080",
    [string]$AdminUsername = "37430534@qq.com",
    [string]$SecondaryUsername = "544481521@qq.com",
    [int]$BatchCopies = 4
)

$ErrorActionPreference = "Stop"
$script:AdminToken = ""
$script:SecondaryToken = ""
$script:KnowledgeBases = [System.Collections.Generic.List[object]]::new()
$script:StoppedContainers = [System.Collections.Generic.HashSet[string]]::new()
$script:SecondaryOriginalHash = ""
$script:PoolBackup = $null
$script:PoolCurrentVersion = 0
$script:Passed = [System.Collections.Generic.List[string]]::new()
$runId = Get-Date -Format "yyyyMMddHHmmss"
$marker = "RUNTIME-STAGE3-$runId"

$apiReplicas = @(
    "weknora-runtime-api-1",
    "weknora-runtime-api-2",
    "weknora-runtime-api-3"
)
$parseWorkers = @(
    "weknora-runtime-parse-1",
    "weknora-runtime-parse-2"
)
$derivativeWorkers = @(
    "weknora-runtime-derivative-1",
    "weknora-runtime-derivative-2"
)
$wikiWorkers = @(
    "weknora-runtime-wiki-1",
    "weknora-runtime-wiki-2"
)
$maintenanceWorkers = @(
    "weknora-runtime-maintenance-1",
    "weknora-runtime-maintenance-2"
)
$docreaders = @(
    "WeKnora-docreader-dev",
    "weknora-runtime-docreader-2",
    "weknora-runtime-docreader-3"
)
$generalAgents = @(
    "weknora-general-agent-e2e-1",
    "weknora-general-agent-e2e-2"
)
$documentAgents = @(
    "weknora-document-agent-e2e-1",
    "weknora-document-agent-e2e-2"
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
        [string]$Token = $script:AdminToken,
        [switch]$Anonymous,
        [hashtable]$ExtraHeaders = @{}
    )
    $headers = @{}
    if (-not $Anonymous -and -not [string]::IsNullOrWhiteSpace($Token)) {
        $headers.Authorization = "Bearer $Token"
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
        $arguments.Body = $Body | ConvertTo-Json -Depth 20 -Compress
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

function Invoke-Db {
    param([string]$Sql)
    $output = & docker exec WeKnora-postgres-dev psql -U postgres -d WeKnora -At -c $Sql
    if ($LASTEXITCODE -ne 0) {
        throw "database query failed"
    }
    return @($output | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
}

function Invoke-DbScalar {
    param([string]$Sql)
    $rows = @(Invoke-Db -Sql $Sql)
    if ($rows.Count -eq 0) {
        return ""
    }
    return [string]$rows[0]
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

function Set-LocalPassword {
    param([string]$Username, [switch]$PreserveOriginal)
    $safeUsername = $Username.Replace("'", "''")
    $original = [string](Invoke-DbScalar -Sql "
        SELECT password_hash FROM users
        WHERE username = '$safeUsername' AND deleted_at IS NULL AND is_active = true
        LIMIT 1;
    ")
    Assert-True (-not [string]::IsNullOrWhiteSpace($original)) "local test user $Username exists and is active"
    $password = "LocalStage3!" + [Guid]::NewGuid().ToString("N").Substring(0, 18)
    $hash = & python -c "import bcrypt,sys; print(bcrypt.hashpw(sys.argv[1].encode(), bcrypt.gensalt()).decode())" $password
    Assert-True ($LASTEXITCODE -eq 0 -and -not [string]::IsNullOrWhiteSpace($hash)) "local bcrypt hash was generated"
    $updated = Invoke-Db -Sql "
        UPDATE users SET password_hash = '$hash', updated_at = NOW()
        WHERE username = '$safeUsername' AND deleted_at IS NULL AND is_active = true
        RETURNING id;
    "
    Assert-True (($updated | Where-Object { $_ -match "^[0-9a-f-]{36}$" }).Count -eq 1) "local password was updated for $Username"
    return [pscustomobject]@{
        Password = $password
        OriginalHash = if ($PreserveOriginal) { $original } else { "" }
    }
}

function Login-User {
    param([string]$Username, [string]$Password)
    $challenge = Get-AuthChallenge
    $encrypted = Protect-Password -PlainText $Password -PublicKey $challenge.PublicKey
    $response = Invoke-Api -Method Post -Path "/api/v1/auth/login" -Anonymous -Body @{
        username = $Username
        encrypted_password = $encrypted
        challenge_id = $challenge.Id
        captcha_answer = $challenge.Answer
    }
    Assert-True ($response.Json.success -eq $true) "encrypted login succeeds for $Username"
    Assert-True (-not [string]::IsNullOrWhiteSpace($response.Json.token)) "login returns a bearer token for $Username"
    return [string]$response.Json.token
}

function Wait-ContainerReady {
    param([string[]]$Names, [int]$TimeoutSeconds = 90)
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $pending = @()
        foreach ($name in $Names) {
            $state = & docker inspect $name --format "{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}" 2>$null
            if ($LASTEXITCODE -ne 0 -or ($state -ne "healthy" -and $state -ne "running")) {
                $pending += "$name=$state"
            }
        }
        if ($pending.Count -eq 0) {
            return
        }
        Start-Sleep -Seconds 2
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "containers did not become ready: $($pending -join ', ')"
}

function Wait-HttpStatus {
    param(
        [string]$Url,
        [int]$ExpectedStatus = 200,
        [int]$TimeoutSeconds = 60
    )
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        try {
            $response = Invoke-WebRequest -Uri $Url -SkipHttpErrorCheck -TimeoutSec 5
            if ($response.StatusCode -eq $ExpectedStatus) {
                return $response
            }
        }
        catch {
            # The load balancer may briefly have no connectable upstream while
            # freshly-started replicas initialize.
        }
        Start-Sleep -Seconds 1
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Url did not reach HTTP $ExpectedStatus within $TimeoutSeconds seconds"
}

function Stop-LocalContainers {
    param([string[]]$Names)
    if ($Names.Count -eq 0) {
        return
    }
    & docker stop --time 30 @Names | Out-Null
    Assert-True ($LASTEXITCODE -eq 0) "local containers stopped: $($Names -join ', ')"
    foreach ($name in $Names) {
        [void]$script:StoppedContainers.Add($name)
    }
}

function Start-LocalContainers {
    param([string[]]$Names, [int]$TimeoutSeconds = 90)
    if ($Names.Count -eq 0) {
        return
    }
    & docker start @Names | Out-Null
    Assert-True ($LASTEXITCODE -eq 0) "local containers restarted: $($Names -join ', ')"
    Wait-ContainerReady -Names $Names -TimeoutSeconds $TimeoutSeconds
    foreach ($name in $Names) {
        [void]$script:StoppedContainers.Remove($name)
    }
}

function Assert-APIHealthy {
    param([int]$Count = 6)
    1..$Count | ForEach-Object {
        $health = Invoke-Api -Method Get -Path "/health" -Anonymous
        Assert-True ($health.Json.status -eq "ok") "API health probe $_ succeeds"
        Assert-True (
            -not ($health.Json.PSObject.Properties.Name -contains "role")
        ) "load balancer health probe $_ is served by an API replica"
    }
}

function New-KnowledgeBase {
    param(
        [string]$Owner,
        [string]$Token,
        [string]$EmbeddingModelID,
        [string]$ConversationModelID,
        [string]$DerivativeModelID,
        [bool]$WikiEnabled,
        [bool]$GraphEnabled
    )
    $response = Invoke-Api -Method Post -Path "/api/v1/knowledge-bases" -Token $Token -ExpectedStatus 201 -Body @{
        name = "Runtime-stage3-$runId-$Owner-$($script:KnowledgeBases.Count + 1)"
        type = "document"
        embedding_model_id = $EmbeddingModelID
        summary_model_id = $ConversationModelID
        derivative_model_id = $DerivativeModelID
        storage_provider_config = @{ provider = "local" }
        question_generation_config = @{ enabled = $true; question_count = 1 }
        indexing_strategy = @{
            vector_enabled = $true
            keyword_enabled = $true
            wiki_enabled = $WikiEnabled
            graph_enabled = $GraphEnabled
        }
    }
    $entry = [pscustomobject]@{
        Id = [string]$response.Json.data.id
        Owner = $Owner
        Token = $Token
        DerivativeModelID = $DerivativeModelID
        WikiEnabled = $WikiEnabled
        GraphEnabled = $GraphEnabled
    }
    $script:KnowledgeBases.Add($entry)
    return $entry
}

function Get-ModelSelection {
    param([string]$Token)
    $models = @((Invoke-Api -Method Get -Path "/api/v1/models" -Token $Token).Json.data)
    $embedding = $models | Where-Object { $_.type -eq "Embedding" } | Select-Object -First 1
    Assert-True ($null -ne $embedding) "tenant has an embedding model"
    $interactive = $models | Where-Object {
        $_.type -eq "KnowledgeQA" -and $_.workload_scope -ne "derivative_only"
    } | Select-Object -First 1
    Assert-True ($null -ne $interactive) "tenant has an interactive chat model"
    $preferred = @(
        $models | Where-Object {
            $_.type -eq "KnowledgeQA" -and
            $_.workload_scope -eq "derivative_only" -and
            ($_.id -eq "prod-qwen36-35b-derivative" -or $_.name -eq "Qwen3.6-35B-A3B")
        } | Select-Object -First 1
    )
    $second = $models | Where-Object {
        $_.type -eq "KnowledgeQA" -and
        $_.workload_scope -eq "derivative_only" -and
        $_.id -ne $preferred[0].id -and
        ($_.id -match "qwen36-27b|qwen3-6-27b" -or $_.name -match "Qwen3\.6-27B")
    } | Select-Object -First 1
    if ($preferred.Count -eq 0) {
        $preferred = @($models | Where-Object {
            $_.type -eq "KnowledgeQA" -and $_.workload_scope -eq "derivative_only"
        } | Select-Object -First 1)
    }
    if ($null -eq $second) {
        $second = $models | Where-Object {
            $_.type -eq "KnowledgeQA" -and
            $_.workload_scope -eq "derivative_only" -and
            $_.id -ne $preferred[0].id
        } | Select-Object -First 1
    }
    $first = if ($preferred.Count -eq 1) { $preferred[0] } else { $null }
    if ($null -eq $second -and $null -ne $first) {
        $second = $first
    }
    return [pscustomobject]@{
        Embedding = $embedding
        Conversation = $interactive
        First = $first
        Second = $second
    }
}

function Pool-Body {
    param([object]$Pool)
    return @{
        id = [string]$Pool.id
        name = [string]$Pool.name
        resource_kind = [string]$Pool.resource_kind
        max_inflight = [int]$Pool.max_inflight
        max_background_inflight = [int]$Pool.max_background_inflight
        interactive_reserve = [int]$Pool.interactive_reserve
        tenant_guaranteed = [int]$Pool.tenant_guaranteed
        tenant_burst = [int]$Pool.tenant_burst
        document_guaranteed = [int]$Pool.document_guaranteed
        document_burst = [int]$Pool.document_burst
        rpm = [int]$Pool.rpm
        tpm = [long]$Pool.tpm
        token_burst = [long]$Pool.token_burst
        request_timeout_seconds = [int]$Pool.request_timeout_seconds
        circuit_threshold = [int]$Pool.circuit_threshold
        circuit_window_seconds = [int]$Pool.circuit_window_seconds
        circuit_open_seconds = [int]$Pool.circuit_open_seconds
        state = [string]$Pool.state
    }
}

function Set-PoolLimit {
    param(
        [object]$Pool,
        [int]$ExpectedVersion,
        [int]$MaxInflight
    )
    $body = Pool-Body -Pool $Pool
    $body.max_inflight = $MaxInflight
    $body.max_background_inflight = $MaxInflight
    $body.interactive_reserve = 0
    $body.tenant_guaranteed = 1
    $body.tenant_burst = $MaxInflight
    $body.document_guaranteed = 1
    $body.document_burst = [Math]::Min($MaxInflight, 2)
    $updated = Invoke-Api -Method Put `
        -Path "/api/v1/custom/derivative-control/resource-pools/$($Pool.id)" `
        -Body $body -ExtraHeaders @{ "If-Match" = "$ExpectedVersion" }
    return $updated.Json.data
}

function Start-RedisPoolSampler {
    param([string]$PoolID, [int]$Seconds)
    Assert-True ($PoolID -match "^[A-Za-z0-9:_-]+$") "resource pool id is safe for local Redis sampling"
    $key = "weknora:model-admission:v2:{$PoolID}:total"
    $envFile = Join-Path $PSScriptRoot "..\..\..\.env"
    $passwordLine = Get-Content -LiteralPath $envFile |
        Where-Object { $_ -match "^REDIS_PASSWORD=" } |
        Select-Object -First 1
    Assert-True (-not [string]::IsNullOrWhiteSpace($passwordLine)) "local Redis password is configured"
    $redisPassword = $passwordLine.Substring("REDIS_PASSWORD=".Length)
    return Start-Job -ScriptBlock {
        param($RedisKey, $Duration, $RedisPassword)
        & docker exec WeKnora-redis-dev sh -lc '
            key="$1"
            duration="$2"
            export REDISCLI_AUTH="$3"
            started="$(date +%s)"
            maximum=0
            samples=0
            while [ $(( $(date +%s) - started )) -lt "$duration" ]; do
                current="$(redis-cli --raw ZCARD "$key" 2>/dev/null || echo 0)"
                case "$current" in (*[!0-9]*|"") current=0;; esac
                if [ "$current" -gt "$maximum" ]; then maximum="$current"; fi
                samples=$((samples + 1))
                sleep 0.1
            done
            printf "%s|%s\n" "$maximum" "$samples"
        ' sampler $RedisKey $Duration $RedisPassword
    } -ArgumentList $key, $Seconds, $redisPassword
}

function Receive-RedisPoolSampler {
    param([System.Management.Automation.Job]$Job)
    Wait-Job -Job $Job | Out-Null
    $line = [string](Receive-Job -Job $Job | Select-Object -Last 1)
    Remove-Job -Job $Job -Force
    Assert-True ($line -match "^(\d+)\|(\d+)$") "Redis pool sampler returned a bounded concurrency observation"
    $parts = $line.Split("|", 2)
    return [pscustomobject]@{
        Maximum = [int]$parts[0]
        Samples = [int]$parts[1]
    }
}

function Upload-One {
    param([string]$Path, [object]$KnowledgeBase)
    $response = Invoke-WebRequest `
        -Uri "$BaseUrl/api/v1/knowledge-bases/$($KnowledgeBase.Id)/knowledge/file" `
        -Method Post `
        -Headers @{ Authorization = "Bearer $($KnowledgeBase.Token)" } `
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
    return [pscustomobject]@{
        Id = [string]$json.data.id
        Owner = $KnowledgeBase.Owner
        Token = $KnowledgeBase.Token
        KnowledgeBaseID = $KnowledgeBase.Id
        Path = $Path
    }
}

function UUID-SqlList {
    param([string[]]$Ids)
    foreach ($id in $Ids) {
        Assert-True ($id -match "^[0-9a-f-]{36}$") "knowledge id is a UUID"
    }
    return ($Ids | ForEach-Object { "'$_'" }) -join ","
}

function Wait-KnowledgeCoreReady {
    param([string[]]$Ids, [int]$TimeoutSeconds = 600)
    $list = UUID-SqlList -Ids $Ids
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $failed = [int](Invoke-DbScalar -Sql "
            SELECT count(*) FROM knowledges
            WHERE id IN ($list) AND (parse_status IN ('failed','cancelled') OR core_status = 'failed');
        ")
        if ($failed -gt 0) {
            throw "$failed knowledge rows failed before core readiness"
        }
        $ready = [int](Invoke-DbScalar -Sql "
            SELECT count(*) FROM knowledges
            WHERE id IN ($list) AND core_status = 'ready';
        ")
        if ($ready -eq $Ids.Count) {
            return
        }
        Start-Sleep -Seconds 3
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "only $ready/$($Ids.Count) knowledge rows reached core-ready"
}

function Wait-KnowledgeTerminal {
    param([string[]]$Ids, [int]$TimeoutSeconds = 900)
    $list = UUID-SqlList -Ids $Ids
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $activeWork = [int](Invoke-DbScalar -Sql "
            SELECT count(*) FROM custom_derivative_work_items
            WHERE knowledge_id IN ($list)
              AND state NOT IN ('completed','failed','provider_unknown','cancelled');
        ")
        $terminal = [int](Invoke-DbScalar -Sql "
            SELECT count(*) FROM knowledges
            WHERE id IN ($list) AND parse_status IN ('completed','failed','cancelled');
        ")
        if ($activeWork -eq 0 -and $terminal -eq $Ids.Count) {
            return
        }
        Start-Sleep -Seconds 4
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "knowledge terminal wait expired: active_work=$activeWork terminal=$terminal/$($Ids.Count)"
}

function Test-AgentReplicaSet {
    param(
        [string]$Name,
        [string[]]$Containers,
        [string]$HealthUrl
    )
    Stop-LocalContainers -Names @($Containers[0])
    $single = Invoke-WebRequest -Uri $HealthUrl -SkipHttpErrorCheck
    Assert-True ($single.StatusCode -eq 200) "$Name stays available after one replica stops"
    Assert-APIHealthy -Count 3
    Start-LocalContainers -Names @($Containers[0])

    Stop-LocalContainers -Names $Containers
    $allDown = Invoke-WebRequest -Uri $HealthUrl -SkipHttpErrorCheck
    Assert-True ($allDown.StatusCode -ge 500) "$Name load balancer reports upstream outage when every replica stops"
    Assert-APIHealthy -Count 4
    Start-LocalContainers -Names $Containers
    $recovered = Wait-HttpStatus -Url $HealthUrl -ExpectedStatus 200 -TimeoutSeconds 60
    Assert-True ($recovered.StatusCode -eq 200) "$Name recovers after every replica restarts"
}

try {
    Assert-APIHealthy -Count 9

    $adminPassword = Set-LocalPassword -Username $AdminUsername
    $script:AdminToken = Login-User -Username $AdminUsername -Password $adminPassword.Password
    $adminPassword = $null
    $secondaryPassword = Set-LocalPassword -Username $SecondaryUsername -PreserveOriginal
    $script:SecondaryOriginalHash = $secondaryPassword.OriginalHash
    $script:SecondaryToken = Login-User -Username $SecondaryUsername -Password $secondaryPassword.Password
    $secondaryPassword = $null

    $adminModels = Get-ModelSelection -Token $script:AdminToken
    $secondaryModels = Get-ModelSelection -Token $script:SecondaryToken
    Assert-True (
        $null -ne $adminModels.First -and
        $null -ne $adminModels.Second -and
        $adminModels.First.id -ne $adminModels.Second.id
    ) "administrator tenant exposes two distinct derivative models for the multi-model run"
    $adminKB1 = New-KnowledgeBase -Owner "admin" -Token $script:AdminToken `
        -EmbeddingModelID $adminModels.Embedding.id -ConversationModelID $adminModels.Conversation.id `
        -DerivativeModelID $adminModels.First.id `
        -WikiEnabled $true -GraphEnabled $true
    $adminKB2 = New-KnowledgeBase -Owner "admin" -Token $script:AdminToken `
        -EmbeddingModelID $adminModels.Embedding.id -ConversationModelID $adminModels.Conversation.id `
        -DerivativeModelID $adminModels.Second.id `
        -WikiEnabled $false -GraphEnabled $false
    $secondaryKB1 = New-KnowledgeBase -Owner "secondary" -Token $script:SecondaryToken `
        -EmbeddingModelID $secondaryModels.Embedding.id -ConversationModelID $secondaryModels.Conversation.id `
        -DerivativeModelID $adminModels.First.id `
        -WikiEnabled $false -GraphEnabled $true
    $secondaryKB2 = New-KnowledgeBase -Owner "secondary" -Token $script:SecondaryToken `
        -EmbeddingModelID $secondaryModels.Embedding.id -ConversationModelID $secondaryModels.Conversation.id `
        -DerivativeModelID $adminModels.Second.id `
        -WikiEnabled $false -GraphEnabled $false
    Pass "two authenticated tenants, four isolated knowledge bases, and multiple selected models"

    Stop-LocalContainers -Names @($apiReplicas[0])
    Assert-APIHealthy -Count 12
    Start-LocalContainers -Names @($apiReplicas[0])
    Pass "single API replica loss and recovery behind the three-replica load balancer"

    $bindings = @((Invoke-Api -Method Get -Path "/api/v1/custom/derivative-control/bindings").Json.data)
    $binding = $bindings | Where-Object {
        $_.model_id -eq $adminModels.First.id -and [long]$_.model_tenant_id -eq 10000
    } | Select-Object -First 1
    Assert-True ($null -ne $binding) "selected derivative model has an exact-route admission binding"
    $pools = @((Invoke-Api -Method Get -Path "/api/v1/custom/derivative-control/resource-pools").Json.data)
    $pool = $pools | Where-Object { $_.id -eq $binding.resource_pool_id } | Select-Object -First 1
    Assert-True ($null -ne $pool) "selected derivative model resource pool exists"
    $script:PoolBackup = $pool
    $limited = Set-PoolLimit -Pool $pool -ExpectedVersion ([int]$pool.policy_version) -MaxInflight 1
    $script:PoolCurrentVersion = [int]$limited.policy_version

    $tempDirectory = Join-Path ([IO.Path]::GetTempPath()) "weknora-runtime-stage3-$runId"
    [IO.Directory]::CreateDirectory($tempDirectory) | Out-Null
    $bundledPython = Join-Path $env:USERPROFILE ".cache\codex-runtimes\codex-primary-runtime\dependencies\python\python.exe"
    Assert-True (Test-Path -LiteralPath $bundledPython) "bundled fixture-generation Python runtime is available"
    $fixtureJson = & $bundledPython `
        custom/tests/runtime_profile_e2e/generate_stage3_fixtures.py `
        --output $tempDirectory --marker $marker --copies $BatchCopies
    Assert-True ($LASTEXITCODE -eq 0) "mixed-format fixtures were generated"
    $fixturePaths = @($fixtureJson | ConvertFrom-Json)
    Assert-True ($fixturePaths.Count -eq (6 * $BatchCopies)) "batch contains six document formats across $BatchCopies copies"

    Stop-LocalContainers -Names @($parseWorkers[0], $docreaders[1], $wikiWorkers[0])
    $kbCycle = @($adminKB1, $adminKB2, $secondaryKB1, $secondaryKB2)
    $uploadSpecs = for ($index = 0; $index -lt $fixturePaths.Count; $index++) {
        [pscustomobject]@{
            Path = [string]$fixturePaths[$index]
            KnowledgeBaseID = [string]$kbCycle[$index % $kbCycle.Count].Id
            Owner = [string]$kbCycle[$index % $kbCycle.Count].Owner
            Token = [string]$kbCycle[$index % $kbCycle.Count].Token
        }
    }
    $uploads = @($uploadSpecs | ForEach-Object -Parallel {
        $spec = $_
        $response = Invoke-WebRequest `
            -Uri "$using:BaseUrl/api/v1/knowledge-bases/$($spec.KnowledgeBaseID)/knowledge/file" `
            -Method Post `
            -Headers @{ Authorization = "Bearer $($spec.Token)" } `
            -Form @{
                file = Get-Item -LiteralPath $spec.Path
                fileName = Split-Path -Leaf $spec.Path
            } `
            -SkipHttpErrorCheck
        if ($response.StatusCode -notin @(200, 201)) {
            throw "parallel upload returned $($response.StatusCode): $($response.Content)"
        }
        $json = $response.Content | ConvertFrom-Json
        [pscustomobject]@{
            Id = [string]$json.data.id
            Owner = $spec.Owner
            Token = $spec.Token
            KnowledgeBaseID = $spec.KnowledgeBaseID
            Path = $spec.Path
        }
    } -ThrottleLimit 12)
    Assert-True ($uploads.Count -eq $fixturePaths.Count) "all mixed-format documents were accepted concurrently"
    $knowledgeIds = @($uploads | ForEach-Object { [string]$_.Id })
    $knowledgeList = UUID-SqlList -Ids $knowledgeIds

    $workDeadline = [DateTime]::UtcNow.AddMinutes(6)
    do {
        $selectedWork = [int](Invoke-DbScalar -Sql "
            SELECT count(*) FROM custom_derivative_work_items
            WHERE knowledge_id IN ($knowledgeList)
              AND resource_pool_id = '$($pool.id)'
              AND work_kind != 'finalizer'
              AND state NOT IN ('completed','failed','provider_unknown','cancelled');
        ")
        if ($selectedWork -ge 4) {
            break
        }
        Start-Sleep -Seconds 2
    } while ([DateTime]::UtcNow -lt $workDeadline)
    Assert-True ($selectedWork -ge 4) "limited model pool received concurrent durable work"

    $lowJob = Start-RedisPoolSampler -PoolID $pool.id -Seconds 18
    $lowSample = Receive-RedisPoolSampler -Job $lowJob
    Write-Host "[INFO] max_inflight=1 observed=$($lowSample.Maximum) samples=$($lowSample.Samples)"
    Assert-True ($lowSample.Maximum -eq 1) "hot policy max_inflight=1 is enforced across derivative replicas"
    $deferred = [int](Invoke-DbScalar -Sql "
        SELECT count(*) FROM custom_derivative_work_items
        WHERE knowledge_id IN ($knowledgeList)
          AND resource_pool_id = '$($pool.id)'
          AND last_error_code = 'model_deferred';
    ")
    Assert-True ($deferred -ge 1) "excess background model work is durably deferred instead of bypassing the limit"

    $expanded = Set-PoolLimit -Pool $limited -ExpectedVersion $script:PoolCurrentVersion -MaxInflight 2
    $script:PoolCurrentVersion = [int]$expanded.policy_version
    $highJob = Start-RedisPoolSampler -PoolID $pool.id -Seconds 24
    $highSample = Receive-RedisPoolSampler -Job $highJob
    Write-Host "[INFO] max_inflight=2 observed=$($highSample.Maximum) samples=$($highSample.Samples)"
    Assert-True ($highSample.Maximum -le 2) "expanded max_inflight=2 remains a hard distributed ceiling"
    Assert-True ($highSample.Maximum -ge 2) "workers consume the hot-expanded second model slot"
    Pass "behavioral hot adjustment of distributed model concurrency from one to two"

    Start-LocalContainers -Names @($parseWorkers[0], $docreaders[1], $wikiWorkers[0])
    Wait-KnowledgeCoreReady -Ids $knowledgeIds -TimeoutSeconds 720
    Pass "$($knowledgeIds.Count)-document concurrent batch across PDF, DOCX, XLSX, CSV, Markdown, and text with one parse/docreader/wiki replica absent"

    $activeBefore = [int](Invoke-DbScalar -Sql "
        SELECT count(*) FROM custom_derivative_work_items
        WHERE knowledge_id IN ($knowledgeList)
          AND state NOT IN ('completed','failed','provider_unknown','cancelled');
    ")
    Assert-True ($activeBefore -gt 0) "derivative batch still has work for replica-failure observation"
    $completedBefore = [int](Invoke-DbScalar -Sql "
        SELECT count(*) FROM custom_derivative_work_items
        WHERE knowledge_id IN ($knowledgeList) AND state = 'completed';
    ")
    Stop-LocalContainers -Names @($derivativeWorkers[0])
    $progressDeadline = [DateTime]::UtcNow.AddSeconds(90)
    do {
        Assert-APIHealthy -Count 1
        $completedAfter = [int](Invoke-DbScalar -Sql "
            SELECT count(*) FROM custom_derivative_work_items
            WHERE knowledge_id IN ($knowledgeList) AND state = 'completed';
        ")
        if ($completedAfter -gt $completedBefore) {
            break
        }
        Start-Sleep -Seconds 4
    } while ([DateTime]::UtcNow -lt $progressDeadline)
    Assert-True ($completedAfter -gt $completedBefore) "remaining derivative replica continues making progress"
    Start-LocalContainers -Names @($derivativeWorkers[0])
    Pass "single derivative replica loss, continued progress, and healthy rejoin"

    Wait-KnowledgeTerminal -Ids $knowledgeIds -TimeoutSeconds 1200
    $failedKnowledge = [int](Invoke-DbScalar -Sql "
        SELECT count(*) FROM knowledges
        WHERE id IN ($knowledgeList) AND parse_status != 'completed';
    ")
    Assert-True ($failedKnowledge -eq 0) "all bulk knowledge rows complete"
    foreach ($kind in @("summary", "question_batch", "graph_batch", "finalizer")) {
        $count = [int](Invoke-DbScalar -Sql "
            SELECT count(*) FROM custom_derivative_work_items
            WHERE knowledge_id IN ($knowledgeList) AND work_kind = '$kind';
        ")
        Assert-True ($count -ge 1) "durable work kind $kind executed"
    }
    $tenantCount = [int](Invoke-DbScalar -Sql "
        SELECT count(DISTINCT tenant_id) FROM custom_derivative_work_items
        WHERE knowledge_id IN ($knowledgeList);
    ")
    $kbCount = [int](Invoke-DbScalar -Sql "
        SELECT count(DISTINCT knowledge_base_id) FROM custom_derivative_work_items
        WHERE knowledge_id IN ($knowledgeList);
    ")
    $modelCount = [int](Invoke-DbScalar -Sql "
        SELECT count(DISTINCT NULLIF(model_id, '')) FROM custom_derivative_work_items
        WHERE knowledge_id IN ($knowledgeList);
    ")
    $duplicateResults = [int](Invoke-DbScalar -Sql "
        SELECT count(*) FROM (
            SELECT work_item_id FROM custom_derivative_results
            WHERE work_item_id IN (
                SELECT id FROM custom_derivative_work_items WHERE knowledge_id IN ($knowledgeList)
            )
            GROUP BY work_item_id HAVING count(*) > 1
        ) AS duplicates;
    ")
    Assert-True ($tenantCount -eq 2) "durable work spans both tenants"
    Assert-True ($kbCount -eq 4) "durable work spans all four knowledge bases"
    Assert-True ($modelCount -ge 2) "durable work resolves multiple model identities"
    Assert-True ($duplicateResults -eq 0) "concurrent execution creates no duplicate immutable results"

    $adminKnowledge = $uploads | Where-Object { $_.Owner -eq "admin" } | Select-Object -First 1
    $crossSecondary = Invoke-Api -Method Get -Path "/api/v1/knowledge/$($adminKnowledge.Id)" `
        -Token $script:SecondaryToken -ExpectedStatus @(403, 404)
    Assert-True ($crossSecondary.Status -in @(403, 404)) "non-admin cross-tenant reads are fenced"
    Pass "multi-tenant/model/KB/task concurrency, result uniqueness, and tenant isolation"

    Stop-LocalContainers -Names @($parseWorkers + $docreaders + $wikiWorkers)
    Assert-APIHealthy -Count 6
    $backlogSource = $fixturePaths |
        Where-Object { [IO.Path]::GetExtension([string]$_) -ieq ".pdf" } |
        Select-Object -First 1
    Assert-True (-not [string]::IsNullOrWhiteSpace($backlogSource)) "PDF outage fixture is available"
    $backlogPath = Join-Path $tempDirectory "full-outage-backlog-$runId.pdf"
    [IO.File]::Copy([string]$backlogSource, $backlogPath, $true)
    $backlog = Upload-One -Path $backlogPath -KnowledgeBase $adminKB1
    Start-Sleep -Seconds 5
    $backlogCore = Invoke-DbScalar -Sql "SELECT core_status FROM knowledges WHERE id = '$($backlog.Id)';"
    Assert-True ($backlogCore -ne "ready") "full parse outage leaves accepted work durable and pending"

    Start-LocalContainers -Names $parseWorkers
    Start-Sleep -Seconds 6
    Assert-APIHealthy -Count 4
    $backlogCore = Invoke-DbScalar -Sql "SELECT core_status FROM knowledges WHERE id = '$($backlog.Id)';"
    Assert-True ($backlogCore -ne "ready") "parse replicas cannot bypass a full docreader outage"

    Start-LocalContainers -Names $docreaders -TimeoutSeconds 120
    Wait-KnowledgeCoreReady -Ids @($backlog.Id) -TimeoutSeconds 600
    $wikiDeadline = [DateTime]::UtcNow.AddMinutes(6)
    do {
        $wikiState = Invoke-DbScalar -Sql "SELECT wiki_status FROM knowledges WHERE id = '$($backlog.Id)';"
        if ($wikiState -eq "pending") {
            break
        }
        Start-Sleep -Seconds 2
    } while ([DateTime]::UtcNow -lt $wikiDeadline)
    Assert-True ($wikiState -eq "pending") "full wiki outage leaves its PostgreSQL intent pending"
    Assert-APIHealthy -Count 4
    Start-LocalContainers -Names $wikiWorkers
    Wait-KnowledgeTerminal -Ids @($backlog.Id) -TimeoutSeconds 900
    $backlogStatus = Invoke-DbScalar -Sql "SELECT parse_status FROM knowledges WHERE id = '$($backlog.Id)';"
    Assert-True ($backlogStatus -eq "completed") "backlog completes after parse, docreader, and wiki roles recover"
    Pass "full parse/docreader/wiki outages, API isolation, durable backlog, and recovery"

    Stop-LocalContainers -Names $maintenanceWorkers
    Assert-APIHealthy -Count 8
    Start-LocalContainers -Names $maintenanceWorkers
    $leaderDeadline = [DateTime]::UtcNow.AddSeconds(30)
    do {
        $leaders = 0
        foreach ($name in $maintenanceWorkers) {
            $leaders += @(& docker logs --since 45s $name 2>&1 |
                Select-String -SimpleMatch "acquired PostgreSQL advisory lock").Count
        }
        if ($leaders -ge 1) {
            break
        }
        Start-Sleep -Seconds 2
    } while ([DateTime]::UtcNow -lt $leaderDeadline)
    Assert-True ($leaders -ge 1) "maintenance leadership is reacquired after a full role outage"
    Pass "full maintenance outage remains isolated from API and recovers leadership"

    Test-AgentReplicaSet -Name "general-agent service" -Containers $generalAgents -HealthUrl "http://localhost:8091/health"
    Test-AgentReplicaSet -Name "document-processing-agent service" -Containers $documentAgents -HealthUrl "http://localhost:8093/health"
    Pass "single/full service-agent outages, API error isolation, and replica recovery"

    $workerMetrics = & docker exec weknora-runtime-derivative-1 sh -lc `
        "curl -fsS http://127.0.0.1:8080/metrics | grep -E 'weknora_model_pool_inflight|weknora_derivative_work_items'"
    Assert-True ($LASTEXITCODE -eq 0 -and @($workerMetrics).Count -ge 1) "worker role exports bounded operational metrics"
    $roleReady = Invoke-WebRequest -Uri "http://localhost:8080/health" -SkipHttpErrorCheck
    Assert-True ($roleReady.StatusCode -eq 200) "API remains ready after the complete fault matrix"
    Pass "dedicated-role metrics and final API stability"

    Write-Host ""
    Write-Host "Stage 3 API/concurrency/fault acceptance passed: $($script:Passed.Count) groups"
}
finally {
    foreach ($name in @($script:StoppedContainers)) {
        try {
            & docker start $name | Out-Null
        }
        catch {
            Write-Warning "failed to restore container $name"
        }
    }
    if ($null -ne $script:PoolBackup -and $script:PoolCurrentVersion -gt 0 -and -not [string]::IsNullOrWhiteSpace($script:AdminToken)) {
        try {
            $body = Pool-Body -Pool $script:PoolBackup
            Invoke-Api -Method Put `
                -Path "/api/v1/custom/derivative-control/resource-pools/$($script:PoolBackup.id)" `
                -Body $body -ExtraHeaders @{ "If-Match" = "$($script:PoolCurrentVersion)" } | Out-Null
        }
        catch {
            Write-Warning "failed to restore resource pool $($script:PoolBackup.id)"
        }
    }
    foreach ($kb in @($script:KnowledgeBases)) {
        try {
            Invoke-Api -Method Delete -Path "/api/v1/knowledge-bases/$($kb.Id)" `
                -Token $kb.Token -ExpectedStatus @(200, 202, 404) | Out-Null
        }
        catch {
            Write-Warning "failed to clean up knowledge base $($kb.Id)"
        }
    }
    if (-not [string]::IsNullOrWhiteSpace($script:SecondaryOriginalHash)) {
        try {
            $safeUsername = $SecondaryUsername.Replace("'", "''")
            Invoke-Db -Sql "
                UPDATE users SET password_hash = '$($script:SecondaryOriginalHash)', updated_at = NOW()
                WHERE username = '$safeUsername' AND deleted_at IS NULL;
            " | Out-Null
        }
        catch {
            Write-Warning "failed to restore secondary local test-user password"
        }
    }
    if (Test-Path variable:tempDirectory) {
        $resolvedTempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
        $resolvedDirectory = [IO.Path]::GetFullPath($tempDirectory)
        if (
            $resolvedDirectory.StartsWith($resolvedTempRoot, [StringComparison]::OrdinalIgnoreCase) -and
            (Split-Path -Leaf $resolvedDirectory).StartsWith("weknora-runtime-stage3-")
        ) {
            Remove-Item -LiteralPath $resolvedDirectory -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}
