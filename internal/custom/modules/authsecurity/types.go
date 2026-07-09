package authsecurity

import "time"

type LoginAttempt struct {
	UsernameKey  string     `json:"username_key" gorm:"primaryKey;size:255"`
	FailedCount  int        `json:"failed_count" gorm:"not null;default:0"`
	LockedUntil  *time.Time `json:"locked_until,omitempty" gorm:"index"`
	LastFailedAt *time.Time `json:"last_failed_at,omitempty"`
	LastIP       string     `json:"last_ip" gorm:"size:64;not null;default:''"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

func (LoginAttempt) TableName() string {
	return "custom_auth_login_attempts"
}

type challengeRecord struct {
	ID                string    `json:"id"`
	PrivateKeyPEM     string    `json:"private_key_pem"`
	CaptchaAnswerHash string    `json:"captcha_answer_hash"`
	ExpiresAt         time.Time `json:"expires_at"`
}

type ChallengeResponse struct {
	ChallengeID      string `json:"challenge_id"`
	PublicKey        string `json:"public_key"`
	CaptchaImage     string `json:"captcha_image"`
	ExpiresInSeconds int    `json:"expires_in_seconds"`
}
