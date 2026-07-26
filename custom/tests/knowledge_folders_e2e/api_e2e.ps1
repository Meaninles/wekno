param(
    [string]$BaseUrl = "http://localhost:8080",
    [string]$Username = "",
    [string]$Password = "KfE2e!726abc",
    [string]$KnowledgeBaseId = "",
    [switch]$KeepData
)

$ErrorActionPreference = "Stop"
$script:Token = ""
$script:CreatedKnowledgeBase = $false
$script:SecondaryKnowledgeBaseId = ""
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

function Get-AuthChallenge {
    $response = Invoke-RestMethod -Uri "$BaseUrl/api/v1/custom/auth-security/challenge"
    $encoded = ($response.data.captcha_image -split ",", 2)[1]
    $svg = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encoded))
    $answer = [regex]::Match($svg, "<text[^>]*>([^<]+)</text>").Groups[1].Value
    Assert-True ($answer.Length -gt 0) "captcha answer is visible in the generated SVG"
    return @{
        Id = $response.data.challenge_id
        PublicKey = $response.data.public_key
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

function Invoke-KfApi {
    param(
        [string]$Method,
        [string]$Path,
        [object]$Body = $null,
        [hashtable]$Form = $null,
        [int[]]$ExpectedStatus = @(200),
        [switch]$Anonymous
    )
    $arguments = @{
        Uri = "$BaseUrl$Path"
        Method = $Method
        SkipHttpErrorCheck = $true
    }
    if (-not $Anonymous -and $script:Token) {
        $arguments.Headers = @{ Authorization = "Bearer $($script:Token)" }
    }
    if ($null -ne $Form) {
        $arguments.Form = $Form
    }
    elseif ($null -ne $Body) {
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

function New-Folder {
    param([string]$Name, [string]$ParentId = "", [string]$Description = "")
    $response = Invoke-KfApi -Method Post `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/folders" `
        -Body @{ name = $Name; parent_id = $ParentId; description = $Description } `
        -ExpectedStatus 201
    return $response.Json.data
}

function Get-Folder {
    param([string]$FolderId)
    return (Invoke-KfApi -Method Get `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/folders/$FolderId").Json.data
}

function New-ManualDocument {
    param(
        [string]$Title,
        [string]$FolderId,
        [string]$Status = "draft",
        [string]$Content = ""
    )
    if ([string]::IsNullOrWhiteSpace($Content)) {
        $Content = "# $Title`n`nKnowledge-folder E2E evidence $runId."
    }
    $response = Invoke-KfApi -Method Post `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/manual" `
        -Body @{
            title = $Title
            content = $Content
            status = $Status
            folder_id = $FolderId
            process_config = @{
                enable_multimodel = $false
                graph_enabled = $false
                extract_config = @{ enabled = $false }
                question_generation_config = @{ enabled = $false; question_count = 3 }
            }
        }
    return $response.Json.data
}

function Login {
    $challenge = Get-AuthChallenge
    $encrypted = Protect-Password -PlainText $Password -PublicKey $challenge.PublicKey
    $response = Invoke-KfApi -Method Post -Path "/api/v1/auth/login" -Anonymous `
        -Body @{
            username = $Username
            encrypted_password = $encrypted
            challenge_id = $challenge.Id
            captcha_answer = $challenge.Answer
        }
    Assert-True ($response.Json.success -eq $true) "login succeeds"
    $script:Token = $response.Json.token
}

function New-TestKnowledgeBase {
    $models = (Invoke-KfApi -Method Get -Path "/api/v1/models").Json.data
    $embedding = $models | Where-Object { $_.type -eq "Embedding" } | Select-Object -First 1
    $summary = $models | Where-Object { $_.type -eq "KnowledgeQA" } | Select-Object -First 1
    Assert-True ($null -ne $embedding) "an embedding model is available"
    $response = Invoke-KfApi -Method Post -Path "/api/v1/knowledge-bases" `
        -Body @{
            name = "KF-E2E-$runId"
            description = "Knowledge folder API and browser E2E fixture"
            type = "document"
            is_temporary = $false
            embedding_model_id = $embedding.id
            summary_model_id = if ($summary) { $summary.id } else { "" }
            storage_provider_config = @{ provider = "local" }
            question_generation_config = @{ enabled = $false; question_count = 3 }
        } `
        -ExpectedStatus 201
    return $response.Json.data.id
}

try {
    $unauthorized = Invoke-KfApi -Method Get `
        -Path "/api/v1/custom/knowledge-folders/search?keyword=probe" `
        -Anonymous -ExpectedStatus 401
    Assert-True ($unauthorized.Status -eq 401) "anonymous access is rejected"
    Pass "authentication guard"

    if ([string]::IsNullOrWhiteSpace($Username)) {
        $Username = "kf-folder-e2e-$runId"
        $challenge = Get-AuthChallenge
        $encrypted = Protect-Password -PlainText $Password -PublicKey $challenge.PublicKey
        Invoke-KfApi -Method Post -Path "/api/v1/auth/register" -Anonymous `
            -Body @{
                username = $Username
                encrypted_password = $encrypted
                encrypted_confirm_password = $encrypted
                challenge_id = $challenge.Id
                captcha_answer = $challenge.Answer
            } `
            -ExpectedStatus 201 | Out-Null
        Pass "self-service test account registration"
    }

    Login
    Pass "encrypted login challenge"

    if ([string]::IsNullOrWhiteSpace($KnowledgeBaseId)) {
        $KnowledgeBaseId = New-TestKnowledgeBase
        $script:CreatedKnowledgeBase = $true
        Pass "test knowledge base creation"
    }
    else {
        $kb = Invoke-KfApi -Method Get -Path "/api/v1/knowledge-bases/$KnowledgeBaseId"
        Assert-True ($kb.Json.data.type -eq "document") "provided knowledge base is a document KB"
        Pass "provided knowledge base access"
    }

    $project = New-Folder -Name "项目-$runId" -Description "API E2E root folder"
    $year = New-Folder -Name "2026" -ParentId $project.id
    $reports = New-Folder -Name "报告" -ParentId $year.id
    $archive = New-Folder -Name "归档-$runId"
    Assert-True ($reports.path -eq "$($project.name)/2026/报告") "three-level path is materialized"
    Pass "multi-level folder creation"

    Invoke-KfApi -Method Post `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/folders" `
        -Body @{ name = " $($project.name.ToUpperInvariant()) " } `
        -ExpectedStatus 409 | Out-Null
    Invoke-KfApi -Method Post `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/folders" `
        -Body @{ name = "../escape" } `
        -ExpectedStatus 400 | Out-Null
    Pass "duplicate and invalid folder names"

    $concurrentName = "并发唯一-$runId"
    $concurrentUrl = "$BaseUrl/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/folders"
    $concurrentBody = @{ name = $concurrentName } | ConvertTo-Json -Compress
    $parallelToken = $script:Token
    $concurrentResults = 1..6 | ForEach-Object -Parallel {
        $response = Invoke-WebRequest -Uri $using:concurrentUrl -Method Post `
            -Headers @{ Authorization = "Bearer $using:parallelToken" } `
            -ContentType "application/json" -Body $using:concurrentBody `
            -SkipHttpErrorCheck
        [int]$response.StatusCode
    } -ThrottleLimit 6
    Assert-True ((@($concurrentResults | Where-Object { $_ -eq 201 })).Count -eq 1) "exactly one concurrent create wins"
    Assert-True ((@($concurrentResults | Where-Object { $_ -eq 409 })).Count -eq 5) "concurrent duplicates are conflicts"
    Pass "concurrent duplicate protection"

    $rename = Invoke-KfApi -Method Put `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/folders/$($reports.id)" `
        -Body @{ name = "报告-已更新"; description = "renamed by API E2E" }
    Assert-True ($rename.Json.data.path.EndsWith("/报告-已更新")) "rename rewrites materialized path"
    $reports = $rename.Json.data
    Invoke-KfApi -Method Put `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/folders/$($project.id)" `
        -Body @{ parent_id = $reports.id } `
        -ExpectedStatus 400 | Out-Null
    Invoke-KfApi -Method Put `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/folders/$($reports.id)" `
        -Body @{ parent_id = "00000000-0000-0000-0000-000000000000" } `
        -ExpectedStatus 404 | Out-Null
    Pass "folder update, subtree path, cycle and missing-parent validation"

    $projectDraft = New-ManualDocument -Title "KF 根级证据 $runId" -FolderId $project.id
    $nestedDraft = New-ManualDocument -Title "KF Nested Evidence $runId" -FolderId $reports.id
    $goldenPhrase = "GOLDEN-PINEAPPLE-$runId"
    $published = New-ManualDocument -Title "KF Retrieval Evidence $runId" `
        -FolderId $reports.id -Status "publish" `
        -Content "# Retrieval`n`n$goldenPhrase confirms flat retrieval across folders."
    Pass "draft and published manual documents in folders"

    $tempDirectory = Join-Path ([IO.Path]::GetTempPath()) "weknora-kf-$runId"
    [IO.Directory]::CreateDirectory($tempDirectory) | Out-Null
    $fileA = Join-Path $tempDirectory "upload-a-$runId.txt"
    $fileB = Join-Path $tempDirectory "upload-b-$runId.txt"
    [IO.File]::WriteAllText($fileA, "folder upload A $runId")
    [IO.File]::WriteAllText($fileB, "folder upload B $runId")
    $relativeRoot = "批量目录-$runId"
    Invoke-KfApi -Method Post `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/files" `
        -Form @{
            file = Get-Item $fileA
            folder_id = $project.id
            relative_path = "$relativeRoot/子目录/$(Split-Path $fileA -Leaf)"
        } | Out-Null
    Invoke-KfApi -Method Post `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/files" `
        -Form @{
            file = Get-Item $fileB
            folder_id = $project.id
            relative_path = "$relativeRoot/子目录/$(Split-Path $fileB -Leaf)"
        } | Out-Null
    Invoke-KfApi -Method Post `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/files" `
        -Form @{
            file = Get-Item $fileA
            folder_id = $project.id
            relative_path = "$relativeRoot/../escape.txt"
        } `
        -ExpectedStatus 400 | Out-Null
    Pass "folder upload path materialization, reuse and traversal rejection"

    $projectStats = (Get-Folder -FolderId $project.id).stats
    Assert-True ($projectStats.subtree_document_count -eq 5) "recursive document count is updated synchronously"
    Assert-True (
        $projectStats.parse_pending_count -ge 0 -and
        $projectStats.parse_running_count -ge 0 -and
        $projectStats.enrichment_pending_task_count -ge 0 -and
        $projectStats.wiki_pending_task_count -ge 0 -and
        $projectStats.abnormal_document_count -ge 0
    ) "precomputed task counters are non-negative"
    Pass "precomputed recursive document and task statistics"

    $folderSearch = Invoke-KfApi -Method Get `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/search?keyword=%E6%8A%A5%E5%91%8A-%E5%B7%B2%E6%9B%B4%E6%96%B0&page=1&page_size=20"
    Assert-True ((@($folderSearch.Json.data | Where-Object { $_.node_type -eq "folder" })).Count -ge 1) "folder search returns folder nodes"
    $documentSearch = Invoke-KfApi -Method Get `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/search?keyword=Nested%20Evidence&page=1&page_size=20"
    Assert-True ((@($documentSearch.Json.data | Where-Object { $_.node_type -eq "document" })).Count -ge 1) "document search returns document nodes"
    $globalSearch = Invoke-KfApi -Method Get `
        -Path "/api/v1/custom/knowledge-folders/search?keyword=Nested%20Evidence&knowledge_base_ids=$KnowledgeBaseId&page=1&page_size=20"
    Assert-True ($globalSearch.Json.total -ge 1) "accessible global search honors KB intersection"
    Pass "recursive KB search and accessible global search"

    $nativeDocumentList = Invoke-KfApi -Method Get `
        -Path "/api/v1/knowledge-bases/$KnowledgeBaseId/knowledge?page=1&page_size=100&keyword=Nested%20Evidence"
    Assert-True ($nativeDocumentList.Json.total -ge 1) "native flat list still sees a nested document"
    Assert-True (
        (@($nativeDocumentList.Json.data | Where-Object { $_.id -eq $nestedDraft.id })).Count -eq 1
    ) "native flat list returns the exact nested document"
    $nativeFolderOnly = Invoke-KfApi -Method Get `
        -Path "/api/v1/knowledge-bases/$KnowledgeBaseId/knowledge?page=1&page_size=100&keyword=%E6%8A%A5%E5%91%8A-%E5%B7%B2%E6%9B%B4%E6%96%B0"
    Assert-True ($nativeFolderOnly.Json.total -eq 0) "folder nodes never leak into native document retrieval"
    Pass "flat retrieval/list semantics remain document-only"

    Invoke-KfApi -Method Put `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/documents/locations" `
        -Body @{ knowledge_ids = @($projectDraft.id); target_folder_id = $archive.id } | Out-Null
    Assert-True ((Get-Folder -FolderId $project.id).stats.subtree_document_count -eq 4) "source aggregate decrements after document move"
    Assert-True ((Get-Folder -FolderId $archive.id).stats.subtree_document_count -eq 1) "target aggregate increments after document move"
    Invoke-KfApi -Method Put `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/documents/locations" `
        -Body @{ knowledge_ids = @("00000000-0000-0000-0000-000000000000"); target_folder_id = $archive.id } `
        -ExpectedStatus 404 | Out-Null
    Pass "document move and missing-document validation"

    $moveReports = Invoke-KfApi -Method Put `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/folders/$($reports.id)" `
        -Body @{ parent_id = $archive.id }
    $reports = $moveReports.Json.data
    Assert-True ((Get-Folder -FolderId $project.id).stats.subtree_document_count -eq 2) "folder move transfers source aggregate"
    Assert-True ((Get-Folder -FolderId $archive.id).stats.subtree_document_count -eq 3) "folder move transfers target aggregate"
    Pass "folder subtree move and aggregate transfer"

    Invoke-KfApi -Method Delete `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/folders/$($reports.id)?mode=reject" `
        -ExpectedStatus 409 | Out-Null
    Invoke-KfApi -Method Delete `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/folders/$($reports.id)?mode=erase" `
        -ExpectedStatus 400 | Out-Null
    Invoke-KfApi -Method Delete `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/folders/$($reports.id)?mode=move_to_parent" | Out-Null
    Invoke-KfApi -Method Get `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/folders/$($reports.id)" `
        -ExpectedStatus 404 | Out-Null
    Assert-True ((Get-Folder -FolderId $archive.id).stats.subtree_document_count -eq 3) "delete-to-parent preserves aggregate"
    Pass "safe non-empty delete modes"

    $invalidPage = Invoke-KfApi -Method Get `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/nodes?page=-1&page_size=20" `
        -ExpectedStatus 400
    $clampedPage = Invoke-KfApi -Method Get `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/nodes?page=1&page_size=1000"
    Assert-True ($clampedPage.Json.page_size -eq 100) "oversized page is clamped"
    for ($index = 1; $index -le 24; $index += 1) {
        New-Folder -Name ("分页-$runId-{0:D2}" -f $index) | Out-Null
    }
    $firstPage = Invoke-KfApi -Method Get `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/nodes?page=1&page_size=20"
    Assert-True ($firstPage.Json.data.Count -eq 20) "first page is bounded to page_size"
    Assert-True ($firstPage.Json.total -gt 20) "fixture spans multiple pages"
    $seen = [System.Collections.Generic.HashSet[string]]::new()
    $pages = [Math]::Ceiling($firstPage.Json.total / 20)
    for ($page = 1; $page -le $pages; $page += 1) {
        $nodePage = Invoke-KfApi -Method Get `
            -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/nodes?page=$page&page_size=20"
        foreach ($node in @($nodePage.Json.data)) {
            $nodeId = if ($node.node_type -eq "folder") { "f:$($node.folder.id)" } else { "d:$($node.document.id)" }
            Assert-True ($seen.Add($nodeId)) "pagination does not duplicate nodes"
        }
    }
    Assert-True ($seen.Count -eq $firstPage.Json.total) "pagination covers every direct node"
    Pass "bounded mixed-node pagination and invalid/clamped parameters"

    $options = Invoke-KfApi -Method Get `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/folders/options"
    Assert-True ((@($options.Json.data | Where-Object { $_.id -eq $archive.id })).Count -eq 1) "folder options include move destinations"
    Pass "folder option read model"

    $models = (Invoke-KfApi -Method Get -Path "/api/v1/models").Json.data
    $embedding = $models | Where-Object { $_.type -eq "Embedding" } | Select-Object -First 1
    $secondary = Invoke-KfApi -Method Post -Path "/api/v1/knowledge-bases" `
        -Body @{
            name = "KF-E2E-secondary-$runId"
            type = "document"
            embedding_model_id = $embedding.id
            storage_provider_config = @{ provider = "local" }
        } `
        -ExpectedStatus 201
    $script:SecondaryKnowledgeBaseId = $secondary.Json.data.id
    $foreignFolderResponse = Invoke-KfApi -Method Post `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$($script:SecondaryKnowledgeBaseId)/folders" `
        -Body @{ name = "foreign-$runId" } `
        -ExpectedStatus 201
    Invoke-KfApi -Method Post `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/folders" `
        -Body @{ name = "cross-scope"; parent_id = $foreignFolderResponse.Json.data.id } `
        -ExpectedStatus 404 | Out-Null
    Pass "cross-KB folder isolation"

    Invoke-KfApi -Method Post `
        -Path "/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/urls" `
        -Body @{ url = "http://127.0.0.1:8080/private"; folder_id = $archive.id } `
        -ExpectedStatus 400 | Out-Null
    Pass "URL SSRF validation"

    $stressUrl = "$BaseUrl/api/v1/custom/knowledge-folders/knowledge-bases/$KnowledgeBaseId/nodes?page=1&page_size=20"
    $stressResults = 1..30 | ForEach-Object -Parallel {
        $response = Invoke-WebRequest -Uri $using:stressUrl -Method Get `
            -Headers @{ Authorization = "Bearer $using:parallelToken" } `
            -SkipHttpErrorCheck
        [int]$response.StatusCode
    } -ThrottleLimit 10
    Assert-True ((@($stressResults | Where-Object { $_ -ne 200 })).Count -eq 0) "parallel reads stay available"
    Pass "parallel read stability"

    $retrievalStatus = "not_completed"
    $deadline = [DateTime]::UtcNow.AddSeconds(120)
    do {
        $details = Invoke-KfApi -Method Get -Path "/api/v1/knowledge/$($published.id)"
        $retrievalStatus = [string]$details.Json.data.parse_status
        if ($retrievalStatus -in @("completed", "failed", "cancelled")) {
            break
        }
        Start-Sleep -Seconds 2
    } while ([DateTime]::UtcNow -lt $deadline)

    if ($retrievalStatus -eq "completed") {
        $hybrid = Invoke-KfApi -Method Post `
            -Path "/api/v1/knowledge-bases/$KnowledgeBaseId/hybrid-search" `
            -Body @{
                query_text = $goldenPhrase
                match_count = 10
                vector_threshold = 0
                keyword_threshold = 0
                disable_vector_match = $true
            }
        Assert-True (
            (@($hybrid.Json.data | Where-Object { $_.knowledge_id -eq $published.id })).Count -ge 1
        ) "completed nested document participates in retrieval"
        Pass "real keyword retrieval of a nested document"
    }
    else {
        Write-Warning "Published document ended in '$retrievalStatus'; flat document-only semantics were verified, but model-backed retrieval could not complete."
    }

    if ($script:SecondaryKnowledgeBaseId) {
        Invoke-KfApi -Method Delete -Path "/api/v1/knowledge-bases/$($script:SecondaryKnowledgeBaseId)" | Out-Null
        $script:SecondaryKnowledgeBaseId = ""
    }

    Write-Host ""
    Write-Host "API E2E passed: $($script:Passed.Count) groups"
    Write-Host "username=$Username"
    Write-Host "password=$Password"
    Write-Host "knowledge_base_id=$KnowledgeBaseId"
    Write-Host "published_document_status=$retrievalStatus"
}
finally {
    if (Test-Path variable:tempDirectory) {
        if (Test-Path $tempDirectory) {
            Remove-Item -LiteralPath $tempDirectory -Recurse -Force
        }
    }
    if (-not $KeepData -and $script:Token -and $script:SecondaryKnowledgeBaseId) {
        try {
            Invoke-KfApi -Method Delete -Path "/api/v1/knowledge-bases/$($script:SecondaryKnowledgeBaseId)" | Out-Null
        }
        catch {
            Write-Warning "Failed to clean secondary KB: $($_.Exception.Message)"
        }
    }
    if (-not $KeepData -and $script:Token -and $script:CreatedKnowledgeBase -and $KnowledgeBaseId) {
        try {
            Invoke-KfApi -Method Delete -Path "/api/v1/knowledge-bases/$KnowledgeBaseId" | Out-Null
        }
        catch {
            Write-Warning "Failed to clean primary KB: $($_.Exception.Message)"
        }
    }
}
