$ErrorActionPreference='Stop'
Set-Location (Split-Path -Parent $PSScriptRoot)
$goCommand = Get-Command go -ErrorAction SilentlyContinue
$goExecutable=if($goCommand){$goCommand.Source}elseif(Test-Path -LiteralPath '..\..\.tools\go\bin\go.exe'){(Resolve-Path -LiteralPath '..\..\.tools\go\bin\go.exe').Path}else{throw 'Install Go 1.27.3, or run the Docker build commands in DEPLOY.md.'}
$env:GOOS='linux';$env:GOARCH='amd64';$env:CGO_ENABLED='0'
foreach($target in @('api','worker','dbops')){
 New-Item -ItemType Directory -Path "build/$target" -Force | Out-Null
 & $goExecutable build -buildvcs=false -trimpath -ldflags '-s -w' -o "build/$target/bootstrap" "./cmd/$target"
 if($LASTEXITCODE -ne 0){throw "Build failed: $target"}
}
New-Item -ItemType Directory -Path build/api/schema,build/dbops/schema -Force | Out-Null
Copy-Item schema/* build/api/schema/ -Force
Copy-Item schema/* build/dbops/schema/ -Force
python scripts/package.py
if($LASTEXITCODE -ne 0){throw 'Packaging failed'}
