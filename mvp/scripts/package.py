"""Package Linux Lambda binaries with executable bootstrap permissions."""
from pathlib import Path
from zipfile import ZipFile, ZipInfo, ZIP_DEFLATED
root=Path(__file__).resolve().parents[1]
for name in ("api","worker","dbops","admin","billing"):
    folder=root/"build"/name
    if not folder.exists(): continue
    with ZipFile(root/"build"/f"{name}.zip","w",ZIP_DEFLATED) as archive:
        for path in folder.rglob("*"):
            if path.is_file():
                info=ZipInfo(path.relative_to(folder).as_posix())
                info.create_system=3
                info.external_attr=(0o100755 if path.name=="bootstrap" else 0o100644)<<16
                archive.writestr(info,path.read_bytes(),compress_type=ZIP_DEFLATED)
    print(f"Packaged {name} with executable bootstrap")
