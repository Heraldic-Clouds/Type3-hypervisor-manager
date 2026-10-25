param([switch]$Execute,[switch]$RemoveImage)
$ErrorActionPreference='Stop'
Set-Location (Split-Path -Parent $PSScriptRoot)
$account=aws sts get-caller-identity --profile stratus2-tantalus --query Account --output text
if($LASTEXITCODE -ne 0 -or $account.Trim() -ne '681773521032'){throw 'Expected deployment account 681773521032'}
$tfArgs=@('run','--rm','-v',"$((Get-Location).Path):/work",'-v',"$env:USERPROFILE\.aws:/root/.aws:ro",'-w','/work/infra','hashicorp/terraform:1.17.0')
docker @tfArgs plan -destroy -input=false '-out=destroy.tfplan'
if($LASTEXITCODE -ne 0){throw 'Destroy plan failed'}
if(!$Execute){Write-Host 'Saved infra/destroy.tfplan. Use -Execute to delete staging and its database.';return}
$instances=aws ec2 describe-instances --profile stratus2-tantalus --filters Name=tag:MEAEnvironment,Values=mea-staging Name=instance-state-name,Values=pending,running,stopping,stopped --query 'Reservations[].Instances[].InstanceId' --output json|ConvertFrom-Json
if($instances.Count -gt 0){
 aws ec2 terminate-instances --instance-ids @instances --profile stratus2-tantalus|Out-Null
 aws ec2 wait instance-terminated --instance-ids @instances --profile stratus2-tantalus
}
aws s3 rm s3://mea-staging-web-681773521032/ --recursive --profile stratus2-tantalus --only-show-errors
if($LASTEXITCODE -ne 0){throw 'Frontend object cleanup failed'}
docker @tfArgs apply -input=false destroy.tfplan
if($LASTEXITCODE -ne 0){throw 'Terraform teardown failed'}
$records=@()
foreach($name in @('miseenaby.me','www.miseenaby.me')){
 $records+=@{Action='UPSERT';ResourceRecordSet=@{Name=$name;Type='A';TTL=300;ResourceRecords=@(@{Value='54.69.123.200'})}}
}
@{Comment='Restore pre-MEA-MVP DNS';Changes=$records}|ConvertTo-Json -Depth 10|Set-Content -LiteralPath build/restore-dns.json -Encoding utf8NoBOM
aws route53 change-resource-record-sets --hosted-zone-id Z2VQV7OZ3AMOWY --change-batch file://build/restore-dns.json --profile default|Out-Null
if($RemoveImage){
 $image=aws ec2 describe-images --image-ids ami-0964ca3ff232953a2 --owners self --profile stratus2-tantalus --output json|ConvertFrom-Json
 if(($image.Images[0].Tags|Where-Object {$_.Key -eq 'Project'}).Value -ne 'MiseEnAbyme'){throw 'AMI ownership tag mismatch'}
 $snapshots=$image.Images[0].BlockDeviceMappings.Ebs.SnapshotId
 aws ec2 deregister-image --image-id ami-0964ca3ff232953a2 --profile stratus2-tantalus
 foreach($snapshot in $snapshots){aws ec2 delete-snapshot --snapshot-id $snapshot --profile stratus2-tantalus}
}
Write-Host 'MEA staging removed; original apex/www DNS restored.'
