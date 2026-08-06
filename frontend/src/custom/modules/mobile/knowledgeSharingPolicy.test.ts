import assert from "node:assert/strict";
import test from "node:test";

import {
  canRemoveKnowledgeBaseShare,
  canShareKnowledgeBase,
  eligibleKnowledgeShareOrganizations,
} from "./knowledgeSharingPolicy.ts";

test("share targets keep owned and admin spaces first and hide viewer-only spaces", () => {
  const rows = eligibleKnowledgeShareOrganizations([
    { id: "viewer", name: "只读空间", my_role: "viewer" },
    { id: "editor", name: "编辑空间", my_role: "editor" },
    { id: "owned", name: "我的空间", is_owner: true },
    { id: "admin", name: "管理空间", my_role: "admin" },
  ] as any);
  assert.deepEqual(rows.map((row) => row.id), ["owned", "admin", "editor"]);
});

test("received knowledge bases cannot be re-shared", () => {
  assert.equal(
    canShareKnowledgeBase({ origin: "organization", creator_id: "me" } as any, {
      currentUserId: "me",
      currentTenantRole: "owner",
    }),
    false,
  );
  assert.equal(
    canShareKnowledgeBase({ origin: "personal", creator_id: "me" } as any, {
      currentUserId: "me",
      currentTenantRole: "contributor",
    }),
    true,
  );
});

test("original sharer, source admin and target-space admin can remove a share", () => {
  const share = {
    shared_by_user_id: "sharer",
    source_tenant_id: 7,
    organization_id: "org-1",
  } as any;
  assert.equal(canRemoveKnowledgeBaseShare(share, { currentUserId: "sharer" }), true);
  assert.equal(canRemoveKnowledgeBaseShare(share, { currentTenantId: 7, currentTenantRole: "admin" }), true);
  assert.equal(canRemoveKnowledgeBaseShare(share, {
    currentUserId: "other",
    currentTenantId: 9,
    currentTenantRole: "contributor",
    organizations: [{ id: "org-1", my_role: "admin" }] as any,
  }), true);
  assert.equal(canRemoveKnowledgeBaseShare(share, {
    currentUserId: "other",
    currentTenantId: 9,
    currentTenantRole: "contributor",
    organizations: [{ id: "org-1", my_role: "editor" }] as any,
  }), false);
});

