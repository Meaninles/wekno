param(
    [string]$BaseUrl = "http://localhost:8080",
    [string]$AdminUsername = "37430534@qq.com"
)

$ErrorActionPreference = "Stop"
$script:OriginalPasswordHash = ""

function Assert-True {
    param([bool]$Condition, [string]$Message)
    if (-not $Condition) {
        throw "Assertion failed: $Message"
    }
}

function Invoke-Api {
    param(
        [string]$Method,
        [string]$Path,
        [object]$Body = $null,
        [hashtable]$Headers = @{}
    )
    $arguments = @{
        Uri = "$BaseUrl$Path"
        Method = $Method
        Headers = $Headers
        SkipHttpErrorCheck = $true
    }
    if ($null -ne $Body) {
        $arguments.ContentType = "application/json"
        $arguments.Body = $Body | ConvertTo-Json -Depth 12 -Compress
    }
    return Invoke-WebRequest @arguments
}

function Get-AuthChallenge {
    $response = Invoke-Api -Method Get -Path "/api/v1/custom/auth-security/challenge"
    Assert-True ($response.StatusCode -eq 200) "local authentication challenge is available"
    $payload = $response.Content | ConvertFrom-Json
    $encoded = ($payload.data.captcha_image -split ",", 2)[1]
    $svg = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encoded))
    $answer = [regex]::Match($svg, "<text[^>]*>([^<]+)</text>").Groups[1].Value
    Assert-True ($answer.Length -gt 0) "local SVG challenge contains an answer"
    return @{
        Id = $payload.data.challenge_id
        PublicKey = $payload.data.public_key
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

function New-LocalAdminLogin {
    $safeUsername = $AdminUsername.Replace("'", "''")
    $original = & docker exec WeKnora-postgres-dev psql -U postgres -d WeKnora -At `
        -c "SELECT password_hash FROM users WHERE username = '$safeUsername' AND deleted_at IS NULL AND is_system_admin = true LIMIT 1;"
    Assert-True (
        $LASTEXITCODE -eq 0 -and
        -not [string]::IsNullOrWhiteSpace([string]$original)
    ) "local system administrator exists"
    $script:OriginalPasswordHash = [string]$original

    $password = "LocalChatQueue!" + [Guid]::NewGuid().ToString("N").Substring(0, 18)
    $hash = & python -c "import bcrypt,sys; print(bcrypt.hashpw(sys.argv[1].encode(), bcrypt.gensalt()).decode())" $password
    Assert-True (
        $LASTEXITCODE -eq 0 -and
        -not [string]::IsNullOrWhiteSpace([string]$hash)
    ) "temporary local password hash was generated"
    $updated = & docker exec WeKnora-postgres-dev psql -U postgres -d WeKnora -At `
        -c "UPDATE users SET password_hash = '$hash', updated_at = NOW() WHERE username = '$safeUsername' AND deleted_at IS NULL AND is_system_admin = true RETURNING id;"
    Assert-True (
        $LASTEXITCODE -eq 0 -and
        (@($updated | Where-Object { $_ -match "^[0-9a-f-]{36}$" })).Count -eq 1
    ) "temporary local administrator password was installed"

    $challenge = Get-AuthChallenge
    $encrypted = Protect-Password -PlainText $password -PublicKey $challenge.PublicKey
    $password = $null
    $response = Invoke-Api -Method Post -Path "/api/v1/auth/login" -Body @{
        username = $AdminUsername
        encrypted_password = $encrypted
        challenge_id = $challenge.Id
        captcha_answer = $challenge.Answer
    }
    Assert-True ($response.StatusCode -eq 200) "encrypted local administrator login succeeds"
    $payload = $response.Content | ConvertFrom-Json
    Assert-True (
        $payload.success -eq $true -and
        -not [string]::IsNullOrWhiteSpace([string]$payload.token)
    ) "local administrator login returns a bearer token"
    return [string]$payload.token
}

function Restore-LocalAdminPassword {
    if ([string]::IsNullOrWhiteSpace($script:OriginalPasswordHash)) {
        return
    }
    $safeUsername = $AdminUsername.Replace("'", "''")
    $safeHash = $script:OriginalPasswordHash.Replace("'", "''")
    & docker exec WeKnora-postgres-dev psql -U postgres -d WeKnora -At `
        -c "UPDATE users SET password_hash = '$safeHash', updated_at = NOW() WHERE username = '$safeUsername' AND deleted_at IS NULL AND is_system_admin = true;" |
        Out-Null
    if ($LASTEXITCODE -ne 0) {
        Write-Warning "failed to restore the original local administrator password hash"
    }
}

try {
    $token = New-LocalAdminLogin
    $env:WEKNORA_QUEUE_E2E_TOKEN = $token
    $env:WEKNORA_QUEUE_E2E_BASE_URL = $BaseUrl
    $token = $null

    & node (Join-Path $PSScriptRoot "chat_queue_e2e.mjs")
    if ($LASTEXITCODE -ne 0) {
        throw "chat queue API acceptance failed with exit code $LASTEXITCODE"
    }
}
finally {
    Remove-Item Env:WEKNORA_QUEUE_E2E_TOKEN -ErrorAction SilentlyContinue
    Remove-Item Env:WEKNORA_QUEUE_E2E_BASE_URL -ErrorAction SilentlyContinue
    Restore-LocalAdminPassword
}
