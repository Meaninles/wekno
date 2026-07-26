package bootstrap

import (
	"os"
	"testing"
)

func TestCustomMigrationsFollowNativeAutoMigrateByDefault(t *testing.T) {
	t.Setenv("AUTO_MIGRATE", "false")
	if customMigrationsEnabled() {
		t.Fatal("custom migrations must be disabled on serving replicas")
	}

	t.Setenv("AUTO_MIGRATE", "true")
	if !customMigrationsEnabled() {
		t.Fatal("custom migrations must run on the maintenance replica")
	}
}

func TestCustomAutoMigrateExplicitOverride(t *testing.T) {
	t.Setenv("AUTO_MIGRATE", "false")
	t.Setenv("CUSTOM_AUTO_MIGRATE", "true")
	if !customMigrationsEnabled() {
		t.Fatal("explicit custom migration enable must override AUTO_MIGRATE")
	}

	t.Setenv("AUTO_MIGRATE", "true")
	t.Setenv("CUSTOM_AUTO_MIGRATE", "false")
	if customMigrationsEnabled() {
		t.Fatal("explicit custom migration disable must override AUTO_MIGRATE")
	}
}

func TestCustomMigrationsRemainEnabledWhenUnset(t *testing.T) {
	originalAuto, hadAuto := os.LookupEnv("AUTO_MIGRATE")
	originalCustom, hadCustom := os.LookupEnv("CUSTOM_AUTO_MIGRATE")
	t.Cleanup(func() {
		if hadAuto {
			_ = os.Setenv("AUTO_MIGRATE", originalAuto)
		} else {
			_ = os.Unsetenv("AUTO_MIGRATE")
		}
		if hadCustom {
			_ = os.Setenv("CUSTOM_AUTO_MIGRATE", originalCustom)
		} else {
			_ = os.Unsetenv("CUSTOM_AUTO_MIGRATE")
		}
	})
	_ = os.Unsetenv("AUTO_MIGRATE")
	_ = os.Unsetenv("CUSTOM_AUTO_MIGRATE")

	if !customMigrationsEnabled() {
		t.Fatal("unset migration flags must retain the existing default-on behavior")
	}
}
