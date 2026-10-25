param(
 [Parameter(Mandatory=$true)][string]$CredentialsFile,
 [string]$SecretName='mea-staging/smtp'
)
$ErrorActionPreference='Stop'
Set-Location (Split-Path -Parent $PSScriptRoot)
$account=aws sts get-caller-identity --profile stratus2-tantalus --query Account --output text
if($LASTEXITCODE -ne 0 -or $account.Trim() -ne '681773521032'){throw 'Expected deployment account 681773521032'}
$credentialsPath=(Resolve-Path -LiteralPath $CredentialsFile).Path
$smtpConfig=Get-Content -LiteralPath $credentialsPath -Raw | ConvertFrom-Json
foreach($field in @('host','username','password','from')){
 if([string]::IsNullOrWhiteSpace($smtpConfig.$field)){throw "SMTP configuration requires $field"}
}
if($smtpConfig.password -eq 'REPLACE_WITH_SMTP_PASSWORD'){throw 'Replace the example password in the private local file first'}
if($smtpConfig.tlsMode -notin @('starttls','tls')){throw 'tlsMode must be starttls or tls'}
if($smtpConfig.port -lt 1 -or $smtpConfig.port -gt 65535){throw 'Invalid SMTP port'}
$secretArn=aws secretsmanager describe-secret --secret-id $SecretName --profile stratus2-tantalus --region us-west-2 --query ARN --output text 2>$null
if($LASTEXITCODE -eq 0){
 aws secretsmanager put-secret-value --secret-id $secretArn --secret-string "file://$credentialsPath" --profile stratus2-tantalus --region us-west-2 --query ARN --output text | Out-Null
}else{
 $secretArn=aws secretsmanager create-secret --name $SecretName --secret-string "file://$credentialsPath" --tags Key=Project,Value=MiseEnAbyme Key=Environment,Value=staging --profile stratus2-tantalus --region us-west-2 --query ARN --output text
}
if($LASTEXITCODE -ne 0){throw 'Unable to store SMTP credentials'}
$smtpConfig=$null
if($secretArn -notmatch '^arn:aws:secretsmanager:us-west-2:681773521032:secret:[A-Za-z0-9/_+=.@-]+$'){throw 'Unexpected SMTP secret ARN'}
$tfvarsPath=Join-Path (Get-Location).Path 'infra/staging.auto.tfvars'
if(!(Test-Path -LiteralPath $tfvarsPath)){Copy-Item infra/staging.tfvars.example $tfvarsPath}
$tfvars=Get-Content -LiteralPath $tfvarsPath -Raw
$setting='smtp_secret_arn = "'+$secretArn.Trim()+'"'
if($tfvars -match '(?m)^smtp_secret_arn\s*=.*$'){
 $tfvars=[regex]::Replace($tfvars,'(?m)^smtp_secret_arn\s*=.*$',$setting)
}else{
 $tfvars=$tfvars.TrimEnd()+[Environment]::NewLine+$setting+[Environment]::NewLine
}
Set-Content -LiteralPath $tfvarsPath -Value $tfvars -Encoding utf8NoBOM
Write-Output "SMTP secret stored: $secretArn"
Write-Output 'Run ./scripts/deploy.ps1 to select SMTP. Keep the local credentials file private or remove it after successful delivery testing.'
