package types

// KnowledgeManagementPermissionSet is the user-facing permission model for a
// knowledge-base-manager agent. Modify is deliberately a derived/composite
// permission: replacing a document always requires both Add and Delete.
type KnowledgeManagementPermissionSet struct {
	Add    bool `yaml:"add" json:"add"`
	Modify bool `yaml:"modify" json:"modify"`
	Delete bool `yaml:"delete" json:"delete"`
}

// Normalize returns a deterministic permission set. Enabling Modify grants
// its two required primitives; having both primitives also means Modify is
// available. There is therefore no state where replace can bypass either
// primitive permission.
func (p KnowledgeManagementPermissionSet) Normalize() KnowledgeManagementPermissionSet {
	if p.Modify {
		p.Add = true
		p.Delete = true
	}
	p.Modify = p.Add && p.Delete
	return p
}

func (p KnowledgeManagementPermissionSet) Any() bool {
	p = p.Normalize()
	return p.Add || p.Delete
}

func (p KnowledgeManagementPermissionSet) Intersect(other KnowledgeManagementPermissionSet) KnowledgeManagementPermissionSet {
	p = p.Normalize()
	other = other.Normalize()
	return KnowledgeManagementPermissionSet{
		Add:    p.Add && other.Add,
		Delete: p.Delete && other.Delete,
	}.Normalize()
}

// KnowledgeManagementConfig stores a uniform permission set plus sparse
// per-KB overrides. An override is authoritative for that KB; every selected
// KB without an override inherits DefaultPermissions.
type KnowledgeManagementConfig struct {
	DefaultPermissions     KnowledgeManagementPermissionSet            `yaml:"default_permissions" json:"default_permissions"`
	KnowledgeBaseOverrides map[string]KnowledgeManagementPermissionSet `yaml:"knowledge_base_overrides,omitempty" json:"knowledge_base_overrides,omitempty"`
}

func (c *KnowledgeManagementConfig) PermissionsFor(kbID string) KnowledgeManagementPermissionSet {
	if c == nil {
		return KnowledgeManagementPermissionSet{}
	}
	if override, ok := c.KnowledgeBaseOverrides[kbID]; ok {
		return override.Normalize()
	}
	return c.DefaultPermissions.Normalize()
}

// KnowledgeManagementRuntimeScope is produced for one chat turn. It is never
// persisted and is the sole authority used by mutation tools.
//
// WholeKnowledgeBaseIDs grant KB-level operations. Documents grants only
// read/replace/delete on the listed documents; it never grants standalone add
// to their parent KB. EffectivePermissions already includes configured
// permissions and the current caller's live platform RBAC.
type KnowledgeManagementRuntimeScope struct {
	ExplicitSelection     bool                                        `json:"explicit_selection"`
	WholeKnowledgeBaseIDs []string                                    `json:"whole_knowledge_base_ids"`
	Documents             map[string]string                           `json:"documents"` // knowledge_id -> knowledge_base_id
	EffectivePermissions  map[string]KnowledgeManagementPermissionSet `json:"effective_permissions"`
	ReadOnlyTagScope      bool                                        `json:"read_only_tag_scope,omitempty"`
}

func (s *KnowledgeManagementRuntimeScope) HasWholeKnowledgeBase(kbID string) bool {
	if s == nil {
		return false
	}
	for _, id := range s.WholeKnowledgeBaseIDs {
		if id == kbID {
			return true
		}
	}
	return false
}

func (s *KnowledgeManagementRuntimeScope) ContainsDocument(knowledgeID, kbID string) bool {
	if s == nil {
		return false
	}
	if s.HasWholeKnowledgeBase(kbID) {
		return true
	}
	return s.Documents[knowledgeID] == kbID
}

func (s *KnowledgeManagementRuntimeScope) PermissionsFor(kbID string) KnowledgeManagementPermissionSet {
	if s == nil {
		return KnowledgeManagementPermissionSet{}
	}
	return s.EffectivePermissions[kbID].Normalize()
}
