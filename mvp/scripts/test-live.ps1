param([string]$Profile='stratus2-tantalus')
$ErrorActionPreference='Stop'
Set-Location (Split-Path -Parent $PSScriptRoot)
$account=aws sts get-caller-identity --profile $Profile --query Account --output text
if($LASTEXITCODE -ne 0 -or $account.Trim() -ne '681773521032'){throw 'Live test requires deployment account 681773521032'}
& ./scripts/build.ps1
$config=aws lambda get-function-configuration --function-name mea-staging-migrate --profile $Profile --output json|ConvertFrom-Json
$create=@{FunctionName='mea-staging-verification-helper';Runtime='provided.al2023';Role=$config.Role;Handler='bootstrap';Timeout=30;MemorySize=256;VpcConfig=@{SubnetIds=$config.VpcConfig.SubnetIds;SecurityGroupIds=$config.VpcConfig.SecurityGroupIds};Environment=$config.Environment;Tags=@{Project='MiseEnAbyme';Purpose='temporary-verification'}}
[IO.File]::WriteAllText((Join-Path (Get-Location) 'build/dbops-config.json'),($create|ConvertTo-Json -Depth 10))
$created=$false
$originalProfile=$env:AWS_PROFILE
try{
 aws lambda create-function --cli-input-json file://build/dbops-config.json --zip-file fileb://build/dbops.zip --profile $Profile --query FunctionName|Out-Null
 if($LASTEXITCODE -ne 0){throw 'Could not create temporary helper; existing helper was not changed'}
 $created=$true
 aws lambda wait function-active --function-name mea-staging-verification-helper --profile $Profile
 if($LASTEXITCODE -ne 0){throw 'Verification helper did not become active'}
 Push-Location ../../mea-web/newbie
 try{
  $env:MEA_LIVE_CONFIRM='YES';$env:AWS_PROFILE=$Profile
  node scripts/live-e2e.mjs
  if($LASTEXITCODE -ne 0){throw 'Live test failed; inspect ignored live-results'}
 }finally{Pop-Location}
}finally{
 if($created){aws lambda delete-function --function-name mea-staging-verification-helper --profile $Profile|Out-Null}
 Remove-Item Env:MEA_LIVE_CONFIRM -ErrorAction SilentlyContinue
 if($null -eq $originalProfile){Remove-Item Env:AWS_PROFILE -ErrorAction SilentlyContinue}else{$env:AWS_PROFILE=$originalProfile}
}
