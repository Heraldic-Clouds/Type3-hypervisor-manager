$ErrorActionPreference='Stop'
Set-Location (Split-Path -Parent $PSScriptRoot)
$settings=@{}
Get-Content -LiteralPath '.env' | ForEach-Object { if($_ -match '^([^=]+)=(.*)$'){$settings[$matches[1]]=$matches[2]} }
foreach($name in @('alice','bob')){
 $body=@{Username=$name;Name=$name;Email="$name@example.test";Password=$settings['SEED_PASSWORD']}|ConvertTo-Json
 try{Invoke-RestMethod -Uri 'http://localhost:8081/api/auth/register' -Method Post -Headers @{Origin='http://localhost:5173'} -ContentType application/json -Body $body|Out-Null}catch{if($_.Exception.Response.StatusCode.value__ -ne 409){throw}}
}
Write-Host 'Seeded alice@example.test and bob@example.test. Password is SEED_PASSWORD in .env.'

