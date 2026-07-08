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
