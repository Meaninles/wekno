#requires -Version 7.0

param(
    [string]$BaseUrl = "http://localhost:8080",
    [string]$AdminUsername = "37430534@qq.com"
)

$ErrorActionPreference = "Stop"
$script:Token = ""
$script:OriginalPasswordHash = ""
$script:Pool = $null
$script:OriginalSchedulerPolicy = $null
$script:SchedulerPolicy = $null
$script:Passed = [System.Collections.Generic.List[string]]::new()
$runId = Get-Date -Format "yyyyMMddHHmmss"

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw "Assertion failed: $Message" }
}

function Pass([string]$Name) {
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
    if (-not $Anonymous -and $script:Token) { $headers.Authorization = "Bearer $($script:Token)" }
    foreach ($entry in $ExtraHeaders.GetEnumerator()) { $headers[$entry.Key] = [string]$entry.Value }
    $args = @{
        Uri = "$BaseUrl$Path"
        Method = $Method
        Headers = $headers
    }
    if ($PSVersionTable.PSVersion.Major -ge 7) {
        $args.SkipHttpErrorCheck = $true
    }
    else {
        $args.UseBasicParsing = $true
    }
    if ($null -ne $Body) {
        $args.ContentType = "application/json"
        $args.Body = $Body | ConvertTo-Json -Depth 20 -Compress
    }
    try {
        $response = Invoke-WebRequest @args
    }
    catch [System.Net.WebException] {
        $httpResponse = $_.Exception.Response
        if ($null -eq $httpResponse) { throw }
        $reader = [System.IO.StreamReader]::new($httpResponse.GetResponseStream())
        try { $content = $reader.ReadToEnd() }
        finally { $reader.Dispose() }
        $response = [pscustomobject]@{
            StatusCode = [int]$httpResponse.StatusCode
            Content = $content
        }
    }
    if ([int]$response.StatusCode -notin $ExpectedStatus) {
        throw "$Method $Path returned $($response.StatusCode): $($response.Content)"
    }
    $json = $null
    if (-not [string]::IsNullOrWhiteSpace($response.Content)) {
        try { $json = $response.Content | ConvertFrom-Json }
        catch { $json = $response.Content }
    }
    return [pscustomobject]@{ Status = [int]$response.StatusCode; Json = $json }
}

function Get-AuthChallenge {
    $response = Invoke-Api -Method Get -Path "/api/v1/custom/auth-security/challenge" -Anonymous
    $encoded = ($response.Json.data.captcha_image -split ",", 2)[1]
    $svg = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encoded))
    return @{
        Id = $response.Json.data.challenge_id
        PublicKey = $response.Json.data.public_key
        Answer = [regex]::Match($svg, "<text[^>]*>([^<]+)</text>").Groups[1].Value
    }
}

function Protect-Password([string]$PlainText, [string]$PublicKey) {
    $rsa = [Security.Cryptography.RSA]::Create()
    try {
        $rsa.ImportFromPem($PublicKey)
        $cipher = $rsa.Encrypt(
            [Text.Encoding]::UTF8.GetBytes($PlainText),
            [Security.Cryptography.RSAEncryptionPadding]::OaepSHA256
        )
        return [Convert]::ToBase64String($cipher)
    }
    finally { $rsa.Dispose() }
}

function Login-LocalAdmin {
    $safeUsername = $AdminUsername.Replace("'", "''")
    $script:OriginalPasswordHash = (& docker exec WeKnora-postgres-dev psql -U postgres -d WeKnora -At `
        -c "SELECT password_hash FROM users WHERE username = '$safeUsername' AND is_system_admin = true LIMIT 1;").Trim()
    Assert-True (-not [string]::IsNullOrWhiteSpace($script:OriginalPasswordHash)) "system administrator exists"
    $password = "CapacityE2E!" + [Guid]::NewGuid().ToString("N").Substring(0, 18)
    $hash = & python -c "import bcrypt,sys; print(bcrypt.hashpw(sys.argv[1].encode(), bcrypt.gensalt()).decode())" $password
    & docker exec WeKnora-postgres-dev psql -U postgres -d WeKnora -v ON_ERROR_STOP=1 -q `
        -c "UPDATE users SET password_hash = '$hash', updated_at = NOW() WHERE username = '$safeUsername';" | Out-Null
    $challenge = Get-AuthChallenge
    $response = Invoke-Api -Method Post -Path "/api/v1/auth/login" -Anonymous -Body @{
        username = $AdminUsername
        encrypted_password = Protect-Password -PlainText $password -PublicKey $challenge.PublicKey
        challenge_id = $challenge.Id
        captcha_answer = $challenge.Answer
    }
    Assert-True ($response.Json.success -eq $true) "system administrator login succeeds"
    $script:Token = [string]$response.Json.token
}

function Restore-LocalAdminPassword {
    if ([string]::IsNullOrWhiteSpace($script:OriginalPasswordHash)) { return }
    $safeUsername = $AdminUsername.Replace("'", "''")
    & docker exec WeKnora-postgres-dev psql -U postgres -d WeKnora -v ON_ERROR_STOP=1 -q `
        -c "UPDATE users SET password_hash = '$($script:OriginalPasswordHash)', updated_at = NOW() WHERE username = '$safeUsername';" | Out-Null
}

function New-PoolBody([int]$Total, [int]$Reserve, [int]$Tenant, [int]$Document, [long]$TPM) {
    return @{
        id = "capacity-e2e-$runId"
        name = "Capacity API acceptance $runId"
        resource_kind = "chat"
        chat_max_concurrent = 999
        chat_max_waiting = 7
        max_inflight = $Total
        max_background_inflight = 999
        interactive_reserve = $Reserve
        tenant_guaranteed = 99
        tenant_burst = $Tenant
        document_guaranteed = 99
        document_burst = $Document
        rpm = 60
        tpm = $TPM
        token_burst = 99999
        request_timeout_seconds = 900
        circuit_threshold = 3
        circuit_window_seconds = 600
        circuit_open_seconds = 60
        state = "enabled"
    }
}

function Assert-Canonical([object]$Pool, [int]$Total, [int]$Reserve, [int]$Tenant, [int]$Document, [long]$TPM) {
    Assert-True ([int]$Pool.max_inflight -eq $Total) "total is effective"
    Assert-True ([int]$Pool.interactive_reserve -eq $Reserve) "reserve is effective"
    Assert-True ([int]$Pool.max_background_inflight -eq ($Total - $Reserve)) "background is derived"
    Assert-True ([int]$Pool.tenant_burst -eq $Tenant) "tenant limit is effective"
    Assert-True ([int]$Pool.document_burst -eq $Document) "document limit is effective"
    Assert-True ([long]$Pool.tpm -eq $TPM) "TPM is effective"
    Assert-True ($null -eq $Pool.chat_max_concurrent) "legacy chat concurrency is cleared"
    Assert-True ([int]$Pool.tenant_guaranteed -eq 1 -and [int]$Pool.document_guaranteed -eq 1) "no-op guarantee fields are canonical"
    Assert-True ([long]$Pool.token_burst -eq 0) "unused token burst is cleared"
}

function Get-WeightedWikiShare([int]$Total, [int]$DerivativeWeight, [int]$WikiWeight) {
    if ($Total -le 0) { return 0 }
    if ($Total -eq 1) { return 1 }
    $derivative = [int][Math]::Ceiling(($Total * $DerivativeWeight) / ($DerivativeWeight + $WikiWeight))
    if ($derivative -lt 1) { $derivative = 1 }
    if ($derivative -ge $Total) { $derivative = $Total - 1 }
    return $Total - $derivative
}

function Get-WikiStageShares([int]$Total) {
    if ($Total -le 0) { return [pscustomobject]@{ Map = 0; Commit = 0 } }
    if ($Total -eq 1) { return [pscustomobject]@{ Map = 1; Commit = 1 } }
    $commit = [Math]::Max(1, [Math]::Floor($Total / 3))
    $map = [Math]::Max(1, $Total - $commit)
    return [pscustomobject]@{ Map = [int]$map; Commit = [int]$commit }
}

try {
    Invoke-Api -Method Get -Path "/health" -Anonymous | Out-Null
    Login-LocalAdmin
    Pass "authenticated capacity-control API"

    $initial = Invoke-Api -Method Get -Path "/api/v1/custom/capacity-control/effective"
    Assert-True ($initial.Json.data.source_of_truth -eq "actual_model_resource_pool") "effective report identifies the source of truth"
    Assert-True ($initial.Json.data.runtime.capacity_wait_counts_as_failure -eq $false) "capacity waits are not failures"
    Assert-True ([int]$initial.Json.data.runtime.background_consumer_slots -ge 1) "worker topology is compiled"
    Assert-True ($initial.Json.data.runtime.instances.Count -ge 1) "live topology comes from instance heartbeats"
    foreach ($row in $initial.Json.data.pools) {
        Assert-True ($null -ne $row.effective.wiki_map_work_share) "Wiki Map work share is compiled"
        Assert-True ($null -ne $row.effective.wiki_commit_work_share) "Wiki Commit work share is compiled"
        Assert-True ($null -ne $row.runtime.work_wiki_map_active) "Wiki Map runtime is observable"
        Assert-True ($null -ne $row.runtime.work_wiki_commit_active) "Wiki Commit runtime is observable"
    }
    Pass "effective policy, Redis runtime, and heartbeat topology report"

    $scheduler = Invoke-Api -Method Get -Path "/api/v1/custom/capacity-control/scheduler-policy"
    $script:OriginalSchedulerPolicy = $scheduler.Json.data.PSObject.Copy()
    $script:SchedulerPolicy = $scheduler.Json.data
    $schedulerVersion = [int]$script:SchedulerPolicy.policy_version
    $invalidSchedulerPolicies = @(
        @{ prefetch_factor = 0; derivative_weight = 2; wiki_weight = 1; background_max_wait_seconds = 30; dispatch_lease_seconds = 120 },
        @{ prefetch_factor = 2; derivative_weight = 0; wiki_weight = 1; background_max_wait_seconds = 30; dispatch_lease_seconds = 120 },
        @{ prefetch_factor = 2; derivative_weight = 2; wiki_weight = 1; background_max_wait_seconds = 0; dispatch_lease_seconds = 120 },
        @{ prefetch_factor = 2; derivative_weight = 2; wiki_weight = 1; background_max_wait_seconds = 30; dispatch_lease_seconds = 20 }
    )
    foreach ($body in $invalidSchedulerPolicies) {
        Invoke-Api -Method Put -Path "/api/v1/custom/capacity-control/scheduler-policy" `
            -Body $body -ExtraHeaders @{ "If-Match" = "`"$schedulerVersion`"" } -ExpectedStatus 400 | Out-Null
    }
    $schedulerAfterReject = Invoke-Api -Method Get -Path "/api/v1/custom/capacity-control/scheduler-policy"
    Assert-True ([int]$schedulerAfterReject.Json.data.policy_version -eq $schedulerVersion) "invalid scheduler writes do not mutate the singleton"
    Pass "scheduler conflict matrix is rejected atomically"

    $schedulerProfiles = @(
        @{ prefetch_factor = 1; derivative_weight = 1; wiki_weight = 1; background_max_wait_seconds = 5; dispatch_lease_seconds = 30 },
        @{ prefetch_factor = 3; derivative_weight = 3; wiki_weight = 1; background_max_wait_seconds = 45; dispatch_lease_seconds = 180 },
        @{ prefetch_factor = 2; derivative_weight = 2; wiki_weight = 1; background_max_wait_seconds = 30; dispatch_lease_seconds = 120 }
    )
    foreach ($profile in $schedulerProfiles) {
        $previousVersion = [int]$script:SchedulerPolicy.policy_version
        $updatedScheduler = Invoke-Api -Method Put -Path "/api/v1/custom/capacity-control/scheduler-policy" `
            -Body $profile -ExtraHeaders @{ "If-Match" = "`"$previousVersion`"" }
        $script:SchedulerPolicy = $updatedScheduler.Json.data
        Assert-True ([int]$script:SchedulerPolicy.policy_version -eq ($previousVersion + 1)) "scheduler policy version advances"
        foreach ($field in $profile.Keys) {
            Assert-True ([int]$script:SchedulerPolicy.$field -eq [int]$profile[$field]) "scheduler field $field is hot"
        }
        1..4 | ForEach-Object {
            $replicaRead = Invoke-Api -Method Get -Path "/api/v1/custom/capacity-control/scheduler-policy"
            Assert-True ([int]$replicaRead.Json.data.policy_version -eq [int]$script:SchedulerPolicy.policy_version) "scheduler is consistent through API replica $_"
        }
    }
    Pass "scheduler profiles hot-update consistently across API replicas"

    $valid = Invoke-Api -Method Post -Path "/api/v1/custom/capacity-control/validate" -Body (New-PoolBody 4 1 4 2 20000)
    Assert-True ($valid.Json.data.valid -eq $true) "valid profile passes"
    Assert-Canonical $valid.Json.data.canonical 4 1 4 2 20000

    $invalidProfiles = @(
        (New-PoolBody 4 4 4 1 20000),
        (New-PoolBody 4 1 5 1 20000),
        (New-PoolBody 4 1 1 2 20000),
        (New-PoolBody 2 1 2 2 20000)
    )
    foreach ($body in $invalidProfiles) {
        $result = Invoke-Api -Method Post -Path "/api/v1/custom/capacity-control/validate" -Body $body
        Assert-True ($result.Json.data.valid -eq $false) "conflicting profile is rejected"
        Assert-True (($result.Json.data.issues | Where-Object { $_.severity -eq "error" }).Count -ge 1) "conflict has an error explanation"
    }
    Pass "reserve, tenant, document, and upstream conflict matrix"

    $created = Invoke-Api -Method Post -Path "/api/v1/custom/capacity-control/resource-pools" `
        -Body (New-PoolBody 4 1 4 2 20000) -ExpectedStatus 201
    $script:Pool = $created.Json.data
    Assert-Canonical $script:Pool 4 1 4 2 20000

    $createdVersion = [int]$script:Pool.policy_version
    foreach ($body in $invalidProfiles) {
        Invoke-Api -Method Put -Path "/api/v1/custom/capacity-control/resource-pools/$($script:Pool.id)" `
            -Body $body -ExtraHeaders @{ "If-Match" = "`"$createdVersion`"" } -ExpectedStatus 400 | Out-Null
    }
    $afterRejectedWrites = Invoke-Api -Method Get -Path "/api/v1/custom/capacity-control/resource-pools"
    $unchangedPool = $afterRejectedWrites.Json.data | Where-Object { $_.id -eq $script:Pool.id } | Select-Object -First 1
    Assert-True ($null -ne $unchangedPool) "pool remains after rejected writes"
    Assert-True ([int]$unchangedPool.policy_version -eq $createdVersion) "rejected writes do not advance policy version"
    Assert-Canonical $unchangedPool 4 1 4 2 20000
    Pass "conflicting writes are rejected without partial mutation"

    $profiles = @(
        @{ Total = 1; Reserve = 0; Tenant = 1; Document = 1; TPM = [long]0 },
        @{ Total = 2; Reserve = 1; Tenant = 1; Document = 1; TPM = [long]20000 },
        @{ Total = 8; Reserve = 2; Tenant = 6; Document = 3; TPM = [long]120000 }
    )
    foreach ($profile in $profiles) {
        $previousVersion = [int]$script:Pool.policy_version
        $updated = Invoke-Api -Method Put -Path "/api/v1/custom/capacity-control/resource-pools/$($script:Pool.id)" `
            -Body (New-PoolBody $profile.Total $profile.Reserve $profile.Tenant $profile.Document $profile.TPM) `
            -ExtraHeaders @{ "If-Match" = "`"$previousVersion`"" }
        $script:Pool = $updated.Json.data
        Assert-True ([int]$script:Pool.policy_version -eq ($previousVersion + 1)) "policy version advances"
        Assert-Canonical $script:Pool $profile.Total $profile.Reserve $profile.Tenant $profile.Document $profile.TPM

        1..4 | ForEach-Object {
            $effective = Invoke-Api -Method Get -Path "/api/v1/custom/capacity-control/effective"
            $row = $effective.Json.data.pools | Where-Object { $_.id -eq $script:Pool.id } | Select-Object -First 1
            Assert-True ($null -ne $row) "profile is visible through load-balanced API replica $_"
            Assert-True ([int]$row.effective.provider_total -eq $profile.Total) "effective total is hot"
            Assert-True ([int]$row.effective.background_max -eq ($profile.Total - $profile.Reserve)) "effective background is hot"
            Assert-True ([int]$row.effective.work_window -eq (($profile.Total - $profile.Reserve) * [int]$script:SchedulerPolicy.prefetch_factor)) "work window follows scheduler prefetch"
            Assert-True ([int]$row.effective.document_max -eq $profile.Document) "effective document cap is hot"

            $background = $profile.Total - $profile.Reserve
            $workWindow = $background * [int]$script:SchedulerPolicy.prefetch_factor
            $wikiWork = Get-WeightedWikiShare $workWindow `
                ([int]$script:SchedulerPolicy.derivative_weight) ([int]$script:SchedulerPolicy.wiki_weight)
            $wikiProvider = Get-WeightedWikiShare $background `
                ([int]$script:SchedulerPolicy.derivative_weight) ([int]$script:SchedulerPolicy.wiki_weight)
            $workStages = Get-WikiStageShares $wikiWork
            $providerStages = Get-WikiStageShares $wikiProvider
            Assert-True ([int]$row.effective.wiki_map_work_share -eq $workStages.Map) "Wiki Map work share is auto-derived"
            Assert-True ([int]$row.effective.wiki_commit_work_share -eq $workStages.Commit) "Wiki Commit work share is auto-derived"
            Assert-True ([int]$row.effective.wiki_map_provider_share -eq $providerStages.Map) "Wiki Map provider share is auto-derived"
            Assert-True ([int]$row.effective.wiki_commit_provider_share -eq $providerStages.Commit) "Wiki Commit provider share is auto-derived"
        }
    }
    Pass "three valid profiles hot-update and automatically recompile stage shares across API replicas"

    Invoke-Api -Method Put -Path "/api/v1/custom/capacity-control/resource-pools/$($script:Pool.id)" `
        -Body (New-PoolBody 4 1 4 2 20000) `
        -ExtraHeaders @{ "If-Match" = '"1"' } -ExpectedStatus 409 | Out-Null
    Pass "stale policy update is rejected atomically"

    Invoke-Api -Method Get -Path "/api/v1/custom/derivative-control/resource-pools" -ExpectedStatus 404 | Out-Null
    Pass "legacy derivative capacity endpoint is removed"
}
finally {
    if ($null -ne $script:Pool) {
        try {
            Invoke-Api -Method Post -Path "/api/v1/custom/capacity-control/resource-pools/$($script:Pool.id)/drain" `
                -ExtraHeaders @{ "If-Match" = "`"$($script:Pool.policy_version)`"" } | Out-Null
            $script:Pool.policy_version = [int]$script:Pool.policy_version + 1
            Invoke-Api -Method Delete -Path "/api/v1/custom/capacity-control/resource-pools/$($script:Pool.id)" `
                -ExtraHeaders @{ "If-Match" = "`"$($script:Pool.policy_version)`"" } | Out-Null
        }
        catch { Write-Warning "capacity test pool cleanup failed: $($_.Exception.Message)" }
    }
    if ($null -ne $script:OriginalSchedulerPolicy -and $null -ne $script:SchedulerPolicy) {
        try {
            Invoke-Api -Method Put -Path "/api/v1/custom/capacity-control/scheduler-policy" `
                -Body $script:OriginalSchedulerPolicy `
                -ExtraHeaders @{ "If-Match" = "`"$($script:SchedulerPolicy.policy_version)`"" } | Out-Null
        }
        catch { Write-Warning "scheduler policy cleanup failed: $($_.Exception.Message)" }
    }
    Restore-LocalAdminPassword
}

Write-Host "Capacity-control API acceptance passed ($($script:Passed.Count) checks):"
$script:Passed | ForEach-Object { Write-Host " - $_" }
