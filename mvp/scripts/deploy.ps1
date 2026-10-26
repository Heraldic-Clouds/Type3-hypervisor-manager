param([switch]$PlanOnly,[string]$ImageTag='newbie-v3')
$ErrorActionPreference='Stop'
Set-Location (Split-Path -Parent $PSScriptRoot)
$account=aws sts get-caller-identity --profile stratus2-tantalus --query Account --output text
if($LASTEXITCODE -ne 0 -or $account.Trim() -ne '681773521032'){throw 'Expected stratus2 deployment account 681773521032'}
if(!(Test-Path -LiteralPath 'infra/staging.auto.tfvars')){Copy-Item infra/staging.tfvars.example infra/staging.auto.tfvars}
New-Item -ItemType Directory -Path build -Force |Out-Null
$functions=aws lambda list-functions --profile stratus2-tantalus --query 'Functions[].FunctionName' --output text
if($functions -match 'mea-staging-control'){
 [IO.File]::WriteAllText((Join-Path (Get-Location) 'build/drain-request.json'),'{"action":"active-sessions"}')
 $metadata=aws lambda invoke --function-name mea-staging-control --payload fileb://build/drain-request.json --profile stratus2-tantalus build/drain-result.json --output json|ConvertFrom-Json
 if($metadata.FunctionError){throw 'Unable to verify active sessions; deployment stopped'}
 $drain=Get-Content build/drain-result.json -Raw|ConvertFrom-Json
 if($drain.count -gt 0){throw 'Active desktops are connected. Retry deployment after users disconnect.'}
}
& ./scripts/build.ps1
if(!$PlanOnly){
 $published=aws ecr describe-images --repository-name mea-staging-guacamole --image-ids "imageTag=$ImageTag" --profile stratus2-tantalus --query 'imageDetails[0].imageDigest' --output text 2>$null
 if($LASTEXITCODE -ne 0){
  docker build -f guacamole/Dockerfile -t "681773521032.dkr.ecr.us-west-2.amazonaws.com/mea-staging-guacamole:$ImageTag" .
  if($LASTEXITCODE -ne 0){throw 'Guacamole build failed'}
  aws ecr get-login-password --region us-west-2 --profile stratus2-tantalus | docker login --username AWS --password-stdin 681773521032.dkr.ecr.us-west-2.amazonaws.com
  docker push "681773521032.dkr.ecr.us-west-2.amazonaws.com/mea-staging-guacamole:$ImageTag"
  if($LASTEXITCODE -ne 0){throw 'Guacamole push failed'}
 }
}
$tfArgs=@('run','--rm','-v',"$((Get-Location).Path):/work",'-v',"$env:USERPROFILE\.aws:/root/.aws:ro",'-w','/work/infra','hashicorp/terraform:1.17.0')
docker @tfArgs init -input=false
if($LASTEXITCODE -ne 0){throw 'Terraform init failed'}
docker @tfArgs plan -input=false "-var=guac_image_tag=$ImageTag" '-out=staging.tfplan'
if($LASTEXITCODE -ne 0){throw 'Terraform plan failed'}
if($PlanOnly){Write-Host 'Saved infra/staging.tfplan. No infrastructure changes applied.';return}
docker @tfArgs apply -input=false staging.tfplan
if($LASTEXITCODE -ne 0){throw 'Terraform apply failed'}
# Migrations run in one locked transaction; a concurrent watchdog can deadlock it, so retry.
for($try=1;$try -le 3;$try++){
 aws lambda invoke --function-name mea-staging-migrate --profile stratus2-tantalus build/migrate-result.json --output json|Out-Null
 if((Get-Content build/migrate-result.json -Raw|ConvertFrom-Json).ok -eq $true){break}
 if($try -eq 3){throw 'Database migration failed'}
 Start-Sleep -Seconds 5
}
Push-Location ../../mea-web/newbie
try{
 npm ci
 if($LASTEXITCODE -ne 0){throw 'Frontend install failed'}
 npm run build
 if($LASTEXITCODE -ne 0){throw 'Frontend build failed'}
 aws s3 sync dist s3://mea-staging-web-681773521032/ --exclude index.html --cache-control 'public,max-age=31536000,immutable' --profile stratus2-tantalus --no-progress
 aws s3 cp dist/index.html s3://mea-staging-web-681773521032/index.html --cache-control no-cache --content-type text/html --profile stratus2-tantalus --no-progress
 aws cloudfront create-invalidation --distribution-id E2TESIQ92T6R7X --paths /index.html / --profile stratus2-tantalus --query Invalidation.Id
}finally{Pop-Location}
Write-Host 'Deployed https://miseenaby.me'
