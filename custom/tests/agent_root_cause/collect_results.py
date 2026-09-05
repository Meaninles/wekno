import concurrent.futures,hashlib,json,pathlib,runpy,subprocess,sys,urllib.request
ROOT=pathlib.Path(__file__).resolve().parents[3]
OUT=ROOT/".local-data/agent-audit-20260905"
rows=[]
for folder in ("root-cause","root-cause-extra","root-cause-history"):
    for line in (OUT/folder/"component-tests.log").read_text(encoding="utf-8").splitlines():
        if "AUDIT_JSON " in line:rows.append({"log":folder,**json.loads(line.split("AUDIT_JSON ",1)[1])})
source_paths=["internal/custom/modules/dbanalytics/service.go","internal/custom/modules/dbanalytics/connector.go","internal/custom/modules/dbanalytics/tools.go","internal/custom/modules/generalagent/service.go","internal/custom/modules/generalagent/original_inputs.go","internal/custom/modules/conversationmemory/policy.go","internal/application/service/agent_history.go","internal/application/service/chat_pipeline/common.go","internal/application/service/chat_pipeline/query_understand.go","internal/agent/tools/knowledge_search.go","internal/agent/tools/get_document_info.go","internal/agent/tools/param_validate.go","custom/services/general-agent/app/runner.py","custom/services/general-agent/app/final_delivery.py"]
sources={p:hashlib.sha256((ROOT/p).read_bytes()).hexdigest() for p in source_paths}
def health(url):
    try:
        with urllib.request.urlopen(url,timeout=15) as r:return {"url":url,"status":r.status}
    except Exception as e:return {"url":url,"error":str(e)[:100]}
urls=["http://localhost:8080/health","http://localhost:5177","http://localhost:5177/mobile/","http://localhost:18091/health","http://localhost:18093/health","http://localhost:13001/api/public/health"]
with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:services=list(pool.map(health,urls))
names=subprocess.run(["docker","ps","--format","{{.Names}}"],text=True,capture_output=True,check=True).stdout.splitlines()
inspected=json.loads(subprocess.run(["docker","inspect",*names],text=True,capture_output=True,check=True).stdout)
containers=[{"name":x["Name"].lstrip("/"),"image":x["Image"],"running":x["State"]["Running"],"health":x["State"].get("Health",{}).get("Status")} for x in inspected]
sys.argv=["live_probe.py","inspect"];P=runpy.run_path(str(OUT/"live_probe.py"));C=P["C"]
sources_test=[]
for sid in ("c1500838-40b1-4737-b856-1e5e5bcd990a","af389ffa-042f-4ba7-a216-c180815e7746"):
    response=C.request("POST",f"/custom/db-analytics/sources/{sid}/test",{})
    sources_test.append({"source_id":sid,"success":response.get("success"),"data":response.get("data")})
result={"component_experiments":rows,"source_sha256":sources,"services":services,"containers":containers,"data_source_tests":sources_test,"document_ab":json.loads((OUT/"root-cause-document/summary.json").read_text(encoding="utf-8")),"sdk_lifecycle":json.loads((OUT/"root-cause-sdk-lifecycle/runtime-timing-summary.json").read_text())}
(OUT/"root-cause-evidence-summary.json").write_text(json.dumps(result,ensure_ascii=False,indent=2),encoding="utf-8")
print(json.dumps({"experiments":len(rows),"services":services,"running_containers":len(containers),"unhealthy":[x["name"] for x in containers if x["health"] not in (None,"healthy")],"data_source_tests":sources_test},ensure_ascii=False,indent=2))
