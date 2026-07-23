package service

import "testing"

func TestValidatePasswordComplexityRequiresTwoCharacterTypes(t *testing.T) {
	valid := []string{
		"abcdef12",
		"abcdef!!",
		"123456!!",
		"中文测试!!",
	}
	for _, password := range valid {
		if err := ValidatePasswordComplexity(password); err != nil {
			t.Fatalf("expected %q to be valid, got %v", password, err)
		}
	}

	invalid := []string{
		"abcdefgh",
		"12345678",
		"!!!!!!!!",
		"abc 1234",
		"Ab1!",
	}
	for _, password := range invalid {
		if err := ValidatePasswordComplexity(password); err == nil {
			t.Fatalf("expected %q to be invalid", password)
		}
	}
}

func TestGenerateCompliantRandomPasswordAlwaysMatchesPolicy(t *testing.T) {
	seen := make(map[string]struct{})
	for range 100 {
		password, err := GenerateCompliantRandomPassword()
		if err != nil {
			t.Fatalf("GenerateCompliantRandomPassword returned error: %v", err)
		}
		if err := ValidatePasswordComplexity(password); err != nil {
			t.Fatalf("generated password %q violates policy: %v", password, err)
		}
		if _, exists := seen[password]; exists {
			t.Fatalf("generated duplicate password %q", password)
		}
		seen[password] = struct{}{}
	}
}
