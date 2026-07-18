package types

import "testing"

func TestKnowledgeManagementPermissionNormalizationAndIntersection(t *testing.T) {
	modify := (KnowledgeManagementPermissionSet{Modify: true}).Normalize()
	if !modify.Add || !modify.Modify || !modify.Delete {
		t.Fatalf("modify must expand to add+delete: %+v", modify)
	}
	composite := (KnowledgeManagementPermissionSet{Add: true, Delete: true}).Normalize()
	if !composite.Modify {
		t.Fatalf("add+delete must derive modify: %+v", composite)
	}
	intersection := composite.Intersect(KnowledgeManagementPermissionSet{Add: true})
	if !intersection.Add || intersection.Delete || intersection.Modify {
		t.Fatalf("intersection must not preserve composite modify without delete: %+v", intersection)
	}
}
