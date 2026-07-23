package authsecurity

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) Challenge(c *gin.Context) {
	challenge, err := h.service.NewChallenge(c.Request.Context())
	if err != nil {
		c.Error(apperrors.NewInternalServerError("生成验证码失败").WithDetails(err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    challenge,
	})
}

func (h *Handler) PasswordCapability(c *gin.Context) {
	userID, ok := types.UserIDFromContext(c.Request.Context())
	if !ok || strings.TrimSpace(userID) == "" {
		c.Error(apperrors.NewUnauthorizedError("未登录"))
		return
	}
	capability, err := h.service.GetPasswordCapability(c.Request.Context(), userID)
	if err != nil {
		c.Error(apperrors.NewInternalServerError("读取账号密码能力失败").WithDetails(err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": capability})
}

func (h *Handler) PrepareLogin(c *gin.Context, req *types.LoginRequest) error {
	username := strings.TrimSpace(req.Username)
	if username == "" {
		return apperrors.NewValidationError("Username is required")
	}
	if until, err := h.service.CheckLocked(c.Request.Context(), username); err != nil {
		return apperrors.NewInternalServerError("登录安全校验失败").WithDetails(err.Error())
	} else if until != nil {
		return lockedError(*until)
	}
	if strings.TrimSpace(req.ChallengeID) == "" ||
		strings.TrimSpace(req.EncryptedPassword) == "" ||
		strings.TrimSpace(req.CaptchaAnswer) == "" {
		return apperrors.NewValidationError("请输入验证码并刷新登录挑战后重试")
	}
	password, err := h.service.DecryptPassword(
		c.Request.Context(),
		req.ChallengeID,
		req.EncryptedPassword,
		req.CaptchaAnswer,
	)
	if err != nil {
		return err
	}
	req.Password = password
	return nil
}

func (h *Handler) PreparePasswordChange(c *gin.Context, req *types.ChangePasswordRequest) error {
	if h == nil || h.service == nil || req == nil {
		return apperrors.NewServiceUnavailableError("密码安全校验不可用")
	}
	if strings.TrimSpace(req.ChallengeID) == "" ||
		strings.TrimSpace(req.EncryptedOldPassword) == "" ||
		strings.TrimSpace(req.EncryptedNewPassword) == "" ||
		strings.TrimSpace(req.EncryptedConfirmPassword) == "" ||
		strings.TrimSpace(req.CaptchaAnswer) == "" {
		return apperrors.NewValidationError("请输入验证码并刷新密码安全验证后重试")
	}
	passwords, err := h.service.DecryptPasswords(
		c.Request.Context(),
		req.ChallengeID,
		req.CaptchaAnswer,
		req.EncryptedOldPassword,
		req.EncryptedNewPassword,
		req.EncryptedConfirmPassword,
	)
	if err != nil {
		return err
	}
	if len(passwords) != 3 || passwords[1] != passwords[2] {
		return apperrors.NewValidationError("两次输入的新密码不一致")
	}
	req.OldPassword = passwords[0]
	req.NewPassword = passwords[1]
	return nil
}

func (h *Handler) GuardPasswordChange(ctx context.Context, user *types.User) error {
	if h == nil || h.service == nil || user == nil {
		return apperrors.NewServiceUnavailableError("密码修改策略不可用")
	}
	capability, err := h.service.GetPasswordCapability(ctx, user.ID)
	if err != nil {
		return apperrors.NewInternalServerError("读取账号密码策略失败").WithDetails(err.Error())
	}
	if !capability.CanChangePassword {
		return apperrors.NewForbiddenError(capability.Reason)
	}
	return nil
}

func (h *Handler) PrepareRegister(c *gin.Context, req *types.RegisterRequest) error {
	if h == nil || h.service == nil || req == nil {
		return apperrors.NewInternalServerError("注册安全校验不可用")
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		return apperrors.NewValidationError("Username is required")
	}
	if strings.TrimSpace(req.ChallengeID) == "" ||
		strings.TrimSpace(req.EncryptedPassword) == "" ||
		strings.TrimSpace(req.EncryptedConfirmPassword) == "" ||
		strings.TrimSpace(req.CaptchaAnswer) == "" {
		return apperrors.NewValidationError("请输入验证码并刷新注册挑战后重试")
	}
	passwords, err := h.service.DecryptPasswords(
		c.Request.Context(),
		req.ChallengeID,
		req.CaptchaAnswer,
		req.EncryptedPassword,
		req.EncryptedConfirmPassword,
	)
	if err != nil {
		return err
	}
	if len(passwords) != 2 || passwords[0] != passwords[1] {
		return apperrors.NewValidationError("两次输入的密码不一致")
	}
	req.Password = passwords[0]
	return nil
}

func (h *Handler) RecordLoginResult(c *gin.Context, req *types.LoginRequest, resp *types.LoginResponse, loginErr error) {
	if h == nil || h.service == nil || req == nil {
		return
	}
	if loginErr != nil {
		return
	}
	if resp != nil && resp.Success {
		h.service.RecordSuccess(c.Request.Context(), req.Username)
		return
	}
	if until, err := h.service.RecordFailure(c.Request.Context(), req.Username, c.ClientIP()); err != nil {
		logger.Warnf(c.Request.Context(), "[authsecurity] failed to update login attempt: %v", err)
	} else if until != nil && resp != nil {
		resp.Message = lockedError(*until).Message
	}
}

func lockedError(until time.Time) *apperrors.AppError {
	remaining := time.Until(until)
	minutes := int(math.Ceil(remaining.Minutes()))
	if minutes < 1 {
		minutes = 1
	}
	return apperrors.NewTooManyRequestsError(fmt.Sprintf("账号已被临时锁定，请 %d 分钟后再试", minutes))
}
