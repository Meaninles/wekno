[CmdletBinding()]
param(
    [string]$BaseUrl = "http://localhost:18080",
    [string]$RunnerEnv = (Join-Path $PSScriptRoot "runner.env"),
    [string]$Bindings = (Join-Path $PSScriptRoot "artifacts/production-derived-kb-bindings.v1.json"),
    [string]$Output = (Join-Path $PSScriptRoot "artifacts/production-corpus-anchor-audit.v1.json")
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

function Read-EnvFile {
    param([Parameter(Mandatory)] [string]$Path)
    $values = @{}
    foreach ($line in Get-Content -LiteralPath $Path) {
        if ($line -match '^(?<key>[A-Za-z_][A-Za-z0-9_]*)=(?<value>.*)$') {
            $values[$Matches.key] = $Matches.value
        }
    }
    return $values
}

function Invoke-EvalGet {
    param([Parameter(Mandatory)] [string]$Path)
    return Invoke-RestMethod `
        -Uri ($BaseUrl.TrimEnd("/") + $Path) `
        -Method Get `
        -Headers $script:Headers `
        -TimeoutSec 60
}

function Get-KnowledgeText {
    param([Parameter(Mandatory)] [string]$KnowledgeID)
    $page = 1
    $content = [Collections.Generic.List[string]]::new()
    $count = 0
    do {
        $response = Invoke-EvalGet -Path "/api/v1/chunks/${KnowledgeID}?page=$page&page_size=100"
        $items = @($response.data)
        foreach ($item in $items) {
            if ([string]$item.content) {
                $content.Add([string]$item.content)
            }
        }
        $count += $items.Count
        $total = [int]$response.total
        $page += 1
    } while ($items.Count -gt 0 -and $count -lt $total)
    return [pscustomobject]@{
        ChunkCount = $count
        Content = $content -join [Environment]::NewLine
        Chunks = @($content)
    }
}

if (-not (Test-Path -LiteralPath $RunnerEnv)) {
    throw "runner.env is missing: $RunnerEnv"
}
if (-not (Test-Path -LiteralPath $Bindings)) {
    throw "knowledge-base bindings are missing: $Bindings"
}

$envValues = Read-EnvFile -Path $RunnerEnv
$apiKey = [string]$envValues["WEKNORA_E2E_TENANT_API_KEY"]
if ([string]::IsNullOrWhiteSpace($apiKey)) {
    throw "isolated eval tenant key is missing"
}
$script:Headers = @{ "X-API-Key" = $apiKey }

$capabilities = Invoke-EvalGet -Path "/api/v1/custom/agent-eval/capabilities"
if ($capabilities.data.mode -ne "eval" -or $capabilities.data.capture_policy -ne "full") {
    throw "anchor audit refuses a non-eval or non-full-capture API"
}

$bindingManifest = Get-Content -LiteralPath $Bindings -Raw | ConvertFrom-Json
$checks = [ordered]@{
    "system-construction-policy" = [ordered]@{
        "project-owner" = @("立项部门")
        "user-test" = @("用户测试", "实际测试", "试运行")
        "acceptance-material" = @("验收申请", "验收材料", "验收报告")
        "archive" = @("归档")
    }
    "digital-policies" = [ordered]@{
        "impact-assessment" = @("变更影响评估", "影响评估")
        "approval" = @("联合审批", "审批")
        "requirements-update" = @("需求规格说明书", "项目计划")
        "change-test-report" = @("变更测试报告")
    }
    "smart-moutai" = [ordered]@{
        "year-2024" = @("2024")
        "year-2025" = @("2025")
        "year-2026-first-half" = @("2026年上半年", "上半年工作总结")
        "year-2027" = @("2027")
        "top-plan" = @("产业链数字生态圈", "管控数字化", "产业数字化", "数字化治理")
    }
    "ip-evidence" = [ordered]@{
        "patent-2020" = @("202011593952", "电子标签及其信息传输方法")
        "patent-2010" = @("201010184037", "数据传输加解密方法")
        "patent-2011" = @("201110259552", "具有防转移功能")
        "key-index" = @("keyIndex")
        "random" = @("random")
        "token" = @("token")
        "reverse-uid" = @("reverseUid")
        "raw-data" = @("rawData")
        "raw-data-lock" = @("rawDataLockFlag")
    }
    "cloud-platform" = [ordered]@{
        "openstack" = @("OpenStack", "openstack")
        "cpu" = @("10424")
        "memory" = @("50641")
        "distributed-block" = @("755.12")
        "object-storage" = @("561.02")
        "file-storage" = @("532.87")
        "production-disaster-layout" = @("生产中心", "灾备中心", "17个")
        "systems" = @("130余", "130+")
    }
    "weknora-guide" = [ordered]@{
        "lightweight-skill" = @("轻量")
        "preloaded-skill" = @("预加载")
        "professional-skill" = @("专业")
        "progressive-disclosure" = @("渐进式披露", "按需加载")
        "read-skill" = @("read_skill")
        "execute-script" = @("execute_skill_script")
        "quick-answer" = @("快速问答")
        "general-agent" = @("通用智能体", "General Agent")
    }
}

$failures = [Collections.Generic.List[string]]::new()
$groups = [ordered]@{}
$groupChunks = @{}
foreach ($groupName in $checks.Keys) {
    $binding = $bindingManifest.knowledge_bases.$groupName
    if ($null -eq $binding) {
        throw "binding manifest is missing group $groupName"
    }
    $groupText = [Text.StringBuilder]::new()
    $chunkTexts = [Collections.Generic.List[string]]::new()
    $chunkCount = 0
    foreach ($document in $binding.documents) {
        $result = Get-KnowledgeText -KnowledgeID ([string]$document.knowledge_id)
        $chunkCount += $result.ChunkCount
        [void]$groupText.AppendLine($result.Content)
        foreach ($chunkText in $result.Chunks) {
            $chunkTexts.Add([string]$chunkText)
        }
    }
    $text = $groupText.ToString()
    $anchorResults = [ordered]@{}
    foreach ($anchorName in $checks[$groupName].Keys) {
        $terms = @($checks[$groupName][$anchorName])
        $matched = @($terms | Where-Object { $text.Contains([string]$_, [StringComparison]::OrdinalIgnoreCase) })
        $passed = $matched.Count -gt 0
        $anchorResults[$anchorName] = [ordered]@{
            passed = $passed
            acceptable_terms = $terms
            matched_terms = $matched
        }
        if (-not $passed) {
            $failures.Add("$groupName/$anchorName")
        }
    }
    $groups[$groupName] = [ordered]@{
        knowledge_base_id = [string]$binding.knowledge_base_id
        document_count = @($binding.documents).Count
        chunk_count = $chunkCount
        anchors = $anchorResults
    }
    $groupChunks[$groupName] = @($chunkTexts)
}

$kbVariableToGroup = @{
    '${AGENT_EVAL_KB_SYSTEM_POLICY_ID}' = "system-construction-policy"
    '${AGENT_EVAL_KB_DIGITAL_POLICIES_ID}' = "digital-policies"
    '${AGENT_EVAL_KB_SMART_MOUTAI_ID}' = "smart-moutai"
    '${AGENT_EVAL_KB_IP_EVIDENCE_ID}' = "ip-evidence"
    '${AGENT_EVAL_KB_CLOUD_PLATFORM_ID}' = "cloud-platform"
    '${AGENT_EVAL_KB_WEKNORA_GUIDE_ID}' = "weknora-guide"
}
$datasetAnchorResults = [ordered]@{}
$datasetPaths = @(
    (Join-Path $PSScriptRoot "datasets/production-multiturn-ready.v1.jsonl"),
    (Join-Path $PSScriptRoot "sealed/production-multiturn-holdout.v1.jsonl")
)
foreach ($datasetPath in $datasetPaths) {
    if (-not (Test-Path -LiteralPath $datasetPath)) {
        continue
    }
    foreach ($line in Get-Content -LiteralPath $datasetPath) {
        if ([string]::IsNullOrWhiteSpace($line)) {
            continue
        }
        $case = $line | ConvertFrom-Json
        $kbIDs = @($case.setup.knowledge_base_ids)
        if ($kbIDs.Count -eq 0) {
            continue
        }
        if ($kbIDs.Count -ne 1 -or -not $kbVariableToGroup.ContainsKey([string]$kbIDs[0])) {
            $failures.Add("dataset/$($case.case_id)/knowledge-base-binding")
            continue
        }
        $groupName = [string]$kbVariableToGroup[[string]$kbIDs[0]]
        $chunks = @($groupChunks[$groupName])
        foreach ($turn in $case.turns) {
            foreach ($evidenceAnchor in @($turn.contract.evidence_anchors)) {
                $anyTerms = @($evidenceAnchor.any_of)
                $allTerms = @($evidenceAnchor.all_of)
                $matchingFragments = 0
                foreach ($chunk in $chunks) {
                    $anyOK = $anyTerms.Count -eq 0 -or @(
                        $anyTerms | Where-Object {
                            ([string]$chunk).Contains([string]$_, [StringComparison]::OrdinalIgnoreCase)
                        }
                    ).Count -gt 0
                    $allOK = @(
                        $allTerms | Where-Object {
                            -not ([string]$chunk).Contains([string]$_, [StringComparison]::OrdinalIgnoreCase)
                        }
                    ).Count -eq 0
                    if ($anyOK -and $allOK) {
                        $matchingFragments += 1
                    }
                }
                $minimum = [Math]::Max(1, [int]$evidenceAnchor.min_matching_fragments)
                $passed = $matchingFragments -ge $minimum
                $resultKey = "$($case.case_id)/$($turn.turn_id)/$($evidenceAnchor.anchor_id)"
                $datasetAnchorResults[$resultKey] = [ordered]@{
                    corpus_group = $groupName
                    matching_fragments = $matchingFragments
                    minimum_matching_fragments = $minimum
                    passed = $passed
                }
                if (-not $passed) {
                    $failures.Add("dataset-anchor/$resultKey")
                }
            }
        }
    }
}

$report = [ordered]@{
    schema_version = 1
    status = if ($failures.Count -eq 0) { "READY" } else { "NOT_READY" }
    formal_eval_executed = $false
    corpus_version = [string]$bindingManifest.corpus_version
    binding_identity_sha256 = [string]$bindingManifest.binding_identity_sha256
    groups = $groups
    dataset_evidence_anchors = $datasetAnchorResults
    failures = @($failures)
}
$outputDirectory = Split-Path -Parent ([IO.Path]::GetFullPath($Output))
New-Item -ItemType Directory -Path $outputDirectory -Force | Out-Null
[IO.File]::WriteAllText(
    [IO.Path]::GetFullPath($Output),
    (($report | ConvertTo-Json -Depth 12) + [Environment]::NewLine),
    [Text.UTF8Encoding]::new($false)
)

if ($failures.Count -gt 0) {
    throw "corpus anchor audit failed: $($failures -join ', ')"
}
Write-Host "PRODUCTION_CORPUS_ANCHORS_READY groups=$($groups.Count) formal_eval_executed=false"
