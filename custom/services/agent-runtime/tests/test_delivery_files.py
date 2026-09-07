import io
from zipfile import ZipFile

import pytest
from app.artifact_validation import validate_artifact
from app.kubernetes_workspace import pod_spec
from test_run import request


def test_office_delivery_rejects_incomplete_packages_and_sets_registered_mime():
    content=io.BytesIO()
    with ZipFile(content,"w") as z:
        for path in ("[Content_Types].xml","_rels/.rels","xl/workbook.xml"):
            z.writestr(path,"<root/>")
    assert validate_artifact("结果.xlsx",content.getvalue()).endswith("spreadsheetml.sheet")
    with pytest.raises(Exception): validate_artifact("broken.xlsx",b"not a workbook")
    with pytest.raises(ValueError): validate_artifact("empty.pdf",b"%PDF-1.7")


def test_kubernetes_workspace_has_private_volumes_and_no_application_credentials(monkeypatch):
    monkeypatch.setenv("AGENT_WORKSPACE_IMAGE","workspace:fixture")
    spec=pod_spec(request(),"fixture")["spec"]
    assert spec["automountServiceAccountToken"] is False
    assert all("hostPath" not in v for v in spec["volumes"])
    container=spec["containers"][0]
    assert container["securityContext"]["readOnlyRootFilesystem"] is True
    assert container["env"] == [{"name":"HOME","value":"/workspace"}]
    assert {m["mountPath"] for m in container["volumeMounts"]} == {"/workspace","/control","/tmp"}
