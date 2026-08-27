[CmdletBinding()]
param(
    [ValidateSet("gate", "sealed_holdout")]
    [string]$Split = "gate",
    [switch]$AllowSealed
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$arguments = @{
    Split = $Split
    PreflightOnly = $true
}
if ($AllowSealed) {
    $arguments.AllowSealed = $true
}

& (Join-Path $PSScriptRoot "eval-loop.ps1") @arguments
if ($LASTEXITCODE -ne 0) {
    throw "eval preparation failed with exit code $LASTEXITCODE"
}
