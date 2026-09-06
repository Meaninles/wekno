[CmdletBinding()]
param(
    [ValidateSet("start", "restart", "stop", "status")]
    [string]$Action = "start"
)

$ErrorActionPreference = "Stop"
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "../../..")).Path
$frontendRoot = Join-Path $repoRoot "frontend"
$vitePath = Join-Path $frontendRoot "node_modules/vite/bin/vite.js"
$vitePattern = [regex]::Escape($vitePath.Replace('/', '\'))
$logRoot = Join-Path $repoRoot ".local-data/frontend"

function Get-FrontendProcess {
    Get-CimInstance Win32_Process | Where-Object {
        $_.Name -eq "node.exe" -and $_.CommandLine -and
        $_.CommandLine.Replace('/', '\') -match $vitePattern
    }
}

$running = @(Get-FrontendProcess)
if ($Action -eq "status") {
    if (!$running.Count) { Write-Host "Frontend stopped (5177)"; exit 1 }
    $response = Invoke-WebRequest "http://localhost:5177/src/views/creatChat/creatChat.vue" -TimeoutSec 15
    Write-Host "Frontend ready: http://localhost:5177 (PID $($running.ProcessId -join ','), module HTTP $($response.StatusCode))"
    exit 0
}
if ($Action -in @("stop", "restart")) {
    foreach ($item in $running) { Stop-Process -Id $item.ProcessId -Force }
    $running = @()
    if ($Action -eq "stop") { exit 0 }
}
if ($running.Count) {
    Write-Host "Frontend already running: http://localhost:5177 (PID $($running.ProcessId -join ','))"
    exit 0
}
if (!(Test-Path -LiteralPath $vitePath)) { throw "Frontend dependencies missing. Run npm ci in $frontendRoot first." }
$listener = Get-NetTCPConnection -LocalPort 5177 -State Listen -ErrorAction SilentlyContinue
if ($listener) { throw "Port 5177 is owned by another process; refusing to start a duplicate frontend." }

New-Item -ItemType Directory -Path $logRoot -Force | Out-Null
$nodePath = (Get-Command node -CommandType Application | Select-Object -First 1).Source
& $nodePath (Join-Path $frontendRoot "src/custom/modules/documentPreview/copyPdfJsAssets.mjs")
if ($LASTEXITCODE -ne 0) { throw "PDF asset preparation failed" }
$env:VITE_DEV_PROXY_TARGET = "http://127.0.0.1:8080"
# Windows Start-Process detaches the server from the invoking terminal. The
# absolute Vite path identifies this checkout for stop/restart, and logs survive
# the caller exiting. Do not run the server in a temporary foreground exec cell.
$server = Start-Process -FilePath $nodePath -ArgumentList @(
    "`"$vitePath`"", "--host", "0.0.0.0", "--port", "5177", "--strictPort"
) -WorkingDirectory $frontendRoot -WindowStyle Hidden -PassThru `
  -RedirectStandardOutput (Join-Path $logRoot "vite.stdout.log") `
  -RedirectStandardError (Join-Path $logRoot "vite.stderr.log")
$deadline = (Get-Date).AddSeconds(45)
do {
    if ($server.HasExited) { throw "Frontend exited with code $($server.ExitCode); see $logRoot" }
    try {
        $response = Invoke-WebRequest "http://localhost:5177/src/views/creatChat/creatChat.vue" -TimeoutSec 3
        if ($response.StatusCode -eq 200) {
            Write-Host "Frontend ready: http://localhost:5177 (PID $($server.Id)); logs: $logRoot"
            exit 0
        }
    } catch { Start-Sleep -Milliseconds 300 }
} while ((Get-Date) -lt $deadline)
throw "Frontend did not become ready within 45 seconds; see $logRoot"
