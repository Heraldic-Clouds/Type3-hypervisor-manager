$ErrorActionPreference='Stop'
Set-Location (Split-Path -Parent $PSScriptRoot)
if (!(Test-Path -LiteralPath '.env')) {
 function New-MEASecret { param([int]$Bytes=24) [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes($Bytes)).ToLower() }
 @("LOCAL_DB_PASSWORD=$(New-MEASecret)","LOCAL_DESKTOP_PASSWORD=$(New-MEASecret)","GUAC_JSON_KEY=$(New-MEASecret -Bytes 16)","SEED_PASSWORD=$(New-MEASecret)") | Set-Content -LiteralPath '.env' -Encoding utf8NoBOM
}
docker compose up -d --build
if ($LASTEXITCODE -ne 0) { throw 'Local environment failed to start' }
Write-Host 'MEA local URL: http://localhost:5173'
Write-Host 'Disposable test credentials are stored only in the ignored .env file.'

