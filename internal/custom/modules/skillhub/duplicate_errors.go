package skillhub

import (
	"errors"
	"strings"
)

const (
	duplicateLightweightSkillNameMessage  = "技能名称已存在，请更换名称。"
	duplicateProfessionalSkillNameMessage = "专业技能名称已存在，请更换名称。"
)

var (
	errLightweightSkillNameExists  = errors.New("skillhub: lightweight skill name exists")
	errProfessionalSkillNameExists = errors.New("skillhub: professional skill name exists")
)

func isLightweightSkillNameExistsError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errLightweightSkillNameExists) {
		return true
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "professional skill") || strings.Contains(msg, "custom_professional_skills") ||
		strings.Contains(msg, "idx_custom_professional") {
		return false
	}
	return strings.Contains(msg, "skill name already exists") ||
		strings.Contains(msg, "skill name conflicts with a preloaded skill") ||
		strings.Contains(msg, "idx_custom_skill_global_name") ||
		strings.Contains(msg, "idx_custom_skill_tenant_name") ||
		strings.Contains(msg, "custom_skills.name")
}

func isProfessionalSkillNameExistsError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errProfessionalSkillNameExists) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "professional skill name already exists") ||
		(strings.Contains(msg, "professional skill") && strings.Contains(msg, "already exists")) ||
		strings.Contains(msg, "idx_custom_professional_skill_name") ||
		strings.Contains(msg, "custom_professional_skills.name")
}
