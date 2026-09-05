"""Read-only observation of one explicitly identified audit SDK run."""
import json, os, pathlib, re, shutil, sys, time
run_id=sys.argv[1]
assert re.fullmatch(r"[0-9a-f-]{36}",run_id)
root=pathlib.Path(os.environ.get("GENERAL_AGENT_RUN_ROOT","/tmp/weknora-general-agent-runs")).resolve()
target=(root/run_id).resolve()
assert target.parent==root
out=pathlib.Path("/tmp/weknora-root-cause-capture")/run_id
out.mkdir(parents=True,exist_ok=True)
end=time.monotonic()+240; seen=False; copied={}
while time.monotonic()<end:
    if not target.exists():
        if seen:break
        time.sleep(.15);continue
    seen=True
    for file in target.rglob("*"):
        try:
            if not file.is_file() or file.is_symlink():continue
            rel=file.relative_to(target)
            if "credential" in file.name.lower() or file.name.startswith(".claude.json") or file.name=="settings.json":continue
            if "shell-snapshots" in rel.parts or "backups" in rel.parts:continue
            st=file.stat()
            if st.st_size>12*1024*1024:continue
            state=(st.st_size,st.st_mtime_ns)
            if copied.get(str(rel))==state:continue
            dest=out/rel;dest.parent.mkdir(parents=True,exist_ok=True)
            shutil.copyfile(file,dest);copied[str(rel)]=state
        except (FileNotFoundError,PermissionError):pass
    time.sleep(.15)
(out/"capture-summary.json").write_text(json.dumps({"run_id":run_id,"seen":seen,"files":list(copied)},ensure_ascii=False,indent=2))
print(json.dumps({"run_id":run_id,"captured_files":len(copied)}),flush=True)
