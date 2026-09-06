[CmdletBinding()]
param(
    [string]$InputPath = (Join-Path $PSScriptRoot "artifacts/production-replay-v2/conversations/production-replay-inputs.json"),
    [string]$OutputPath = (Join-Path $PSScriptRoot "artifacts/production-replay-v2/tests/production-equivalent-replay-plan.v1.json")
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

if (-not (Test-Path -LiteralPath $InputPath)) {
    throw "SUT-safe production replay input is missing: $InputPath"
}

$source = Get-Content -Raw -LiteralPath $InputPath | ConvertFrom-Json -Depth 100
foreach ($flag in @(
    "source_answers_in_sut_input",
    "source_references_in_sut_input",
    "reference_answers_in_sut_input",
    "judge_feedback_in_sut_input",
    "sealed_holdout_accessed"
)) {
    if ([bool]$source.$flag) {
        throw "unsafe production replay input: $flag must be false"
    }
}

$supportedKBs = @(
    "expense-ops-qa",
    "imoutai-private-wecom",
    "expense-operation-guide"
)

# All real multi-turn sessions are kept intact. The singleton selection covers
# the distinct production failure/capability families without copying source
# answers or turning one observed answer into a gold answer.
$singleTurnSelection = @(
    @{ kb="expense-ops-qa"; query="报账系统如何提交报销申请？"; family="broad-procedure-retrieval" },
    @{ kb="expense-ops-qa"; query="人力的数据推到报账系统后未显示代扣公司代垫的个人部分社保"; family="specific-troubleshooting" },
    @{ kb="expense-ops-qa"; query="通用报账单的新建流程"; family="focused-procedure-retrieval" },
    @{ kb="expense-ops-qa"; query="爱茅台的通用报账单的新建流程"; family="cross-product-wording" },
    @{ kb="expense-ops-qa"; query="提交与审批怎么操作"; family="underspecified-procedure" },
    @{ kb="expense-ops-qa"; query="报账失败如何处理"; family="broad-troubleshooting" },
    @{ kb="expense-ops-qa"; query="报账系统无法打开"; family="access-troubleshooting" },
    @{ kb="expense-ops-qa"; query="如何发起出差申请单？"; family="procedure-retrieval" },
    @{ kb="expense-ops-qa"; query="我现在有几天年假？"; family="personal-data-boundary" },
    @{ kb="expense-ops-qa"; query="出差可以报销油费吗"; family="evidence-gap" },
    @{ kb="imoutai-private-wecom"; query="爱茅台鸿蒙版本如何下载"; family="known-empty-answer" },
    @{ kb="imoutai-private-wecom"; query="爱茅台登录失败"; family="login-troubleshooting" },
    @{ kb="imoutai-private-wecom"; query="在哪里反馈问题"; family="feedback-procedure" },
    @{ kb="imoutai-private-wecom"; query="爱茅台上怎么预约线上会议"; family="meeting-procedure" },
    @{ kb="imoutai-private-wecom"; query="会议登录如何使用"; family="ambiguous-meeting-procedure" },
    @{ kb="imoutai-private-wecom"; query="日程如何使用"; family="schedule-procedure" },
    @{ kb="imoutai-private-wecom"; query="我有一个系统，如何添加到工作台"; family="admin-boundary" },
    @{ kb="imoutai-private-wecom"; query="可以帮我读取内网文章吗"; family="unavailable-resource-boundary" },
    @{ kb="imoutai-private-wecom"; query="怎么品酒"; family="out-of-domain" }
)

$selected = [Collections.Generic.List[object]]::new()
$selectedAliases = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)

foreach ($session in @($source.sessions)) {
    if ($supportedKBs -notcontains [string]$session.kb_slug) { continue }
    if (@($session.turns).Count -le 1) { continue }
    if (@($session.turns | Where-Object { $_.has_images -or $_.has_attachments }).Count -gt 0) {
        throw "selected multi-turn session needs unavailable binary context: $($session.source_session_alias)"
    }
    $selected.Add([ordered]@{
        source_session_alias = [string]$session.source_session_alias
        source_agent_kind = [string]$session.source_agent_kind
        kb_slug = [string]$session.kb_slug
        family = "production-multiturn"
        high_usage_user = [bool]$session.high_usage_user
        turns = @($session.turns | ForEach-Object {
            [ordered]@{ source_ordinal=[int]$_.source_ordinal; query=[string]$_.query }
        })
    })
    $null = $selectedAliases.Add([string]$session.source_session_alias)
}

foreach ($selector in $singleTurnSelection) {
    $match = @($source.sessions | Where-Object {
        [string]$_.kb_slug -eq [string]$selector.kb -and
        @($_.turns).Count -eq 1 -and
        [string]$_.turns[0].query -eq [string]$selector.query -and
        -not $_.turns[0].has_images -and
        -not $_.turns[0].has_attachments
    } | Sort-Object @{ Expression={ -not [bool]$_.high_usage_user } }, source_session_alias)[0]
    if ($null -eq $match) {
        throw "production replay selector did not match: $($selector.kb) / $($selector.query)"
    }
    if (-not $selectedAliases.Add([string]$match.source_session_alias)) { continue }
    $selected.Add([ordered]@{
        source_session_alias = [string]$match.source_session_alias
        source_agent_kind = [string]$match.source_agent_kind
        kb_slug = [string]$match.kb_slug
        family = [string]$selector.family
        high_usage_user = [bool]$match.high_usage_user
        turns = @([ordered]@{
            source_ordinal = [int]$match.turns[0].source_ordinal
            query = [string]$match.turns[0].query
        })
    })
}

$cases = @()
$index = 0
foreach ($item in $selected) {
    $index++
    $cases += [ordered]@{
        case_id = "prod-replay-{0:d3}" -f $index
        source_session_alias = $item.source_session_alias
        source_agent_kind = $item.source_agent_kind
        kb_slug = $item.kb_slug
        family = $item.family
        high_usage_user = $item.high_usage_user
        turns = $item.turns
    }
}

$plan = [ordered]@{
    schema_version = 1
    generated_at = [DateTimeOffset]::UtcNow.ToString("o")
    source_environment = "weknora-prod"
    target_environment = "local-production-mode"
    purpose = "production-equivalent reproduction and qualitative analysis only"
    required_model_id = "prod-deepseek-v4-flash-int8-chat"
    source_answers_in_sut_input = $false
    source_references_in_sut_input = $false
    reference_answers_in_sut_input = $false
    required_claims_in_sut_input = $false
    judge_feedback_in_sut_input = $false
    sealed_holdout_accessed = $false
    eval_assistance_enabled = $false
    profiles = @("quick-answer", "rag-reasoning", "general-agent")
    repetitions = 3
    concurrency = 12
    cases = $cases
}

$fullOutputPath = [IO.Path]::GetFullPath($OutputPath)
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $fullOutputPath) | Out-Null
[IO.File]::WriteAllText(
    $fullOutputPath,
    ($plan | ConvertTo-Json -Depth 20),
    [Text.UTF8Encoding]::new($false)
)

$turnCount = @($cases | ForEach-Object { @($_.turns).Count } | Measure-Object -Sum).Sum
Write-Host "Production replay plan: cases=$($cases.Count) turns=$turnCount profiles=3 repetitions=3 conversations=$($cases.Count * 9) model=$($plan.required_model_id)"
Write-Host "Plan: $fullOutputPath"
