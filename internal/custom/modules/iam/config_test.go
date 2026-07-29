package iam

import "testing"

func TestNormalizePublicOrigin(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "empty", input: "", want: ""},
		{name: "production https", input: "https://knora.moutai.com.cn/", want: "https://knora.moutai.com.cn"},
		{name: "localhost http", input: "http://localhost:5177", want: "http://localhost:5177"},
		{name: "loopback http", input: "http://127.0.0.1:5177/", want: "http://127.0.0.1:5177"},
		{name: "public http rejected", input: "http://knora.moutai.com.cn", wantErr: true},
		{name: "path rejected", input: "https://knora.moutai.com.cn/mobile", wantErr: true},
		{name: "query rejected", input: "https://knora.moutai.com.cn?client=mobile", wantErr: true},
		{name: "userinfo rejected", input: "https://user@knora.moutai.com.cn", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizePublicOrigin(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizePublicOrigin(%q) returned nil error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizePublicOrigin(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("normalizePublicOrigin(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestLoadPublicOriginUsesEnvironmentFallback(t *testing.T) {
	t.Setenv(frontendBaseURLEnv, "https://knora.moutai.com.cn/")

	got, err := LoadPublicOrigin("")
	if err != nil {
		t.Fatalf("LoadPublicOrigin: %v", err)
	}
	if got != "https://knora.moutai.com.cn" {
		t.Fatalf("LoadPublicOrigin = %q", got)
	}
}

func TestLoadPublicOriginConfiguredValueTakesPrecedence(t *testing.T) {
	t.Setenv(frontendBaseURLEnv, "https://ignored.example.com")

	got, err := LoadPublicOrigin("http://localhost:5177")
	if err != nil {
		t.Fatalf("LoadPublicOrigin: %v", err)
	}
	if got != "http://localhost:5177" {
		t.Fatalf("LoadPublicOrigin = %q", got)
	}
}
