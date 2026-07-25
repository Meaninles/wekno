package llmjson

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestStringListAcceptsScalarArrayAndNull(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want StringList
	}{
		{
			name: "scalar is split trimmed and deduplicated",
			raw:  `"版本管理, 版本迭代，版本管理"`,
			want: StringList{"版本管理", "版本迭代"},
		},
		{
			name: "array is normalized",
			raw:  `["c000", " c001 ", "c000"]`,
			want: StringList{"c000", "c001"},
		},
		{
			name: "null",
			raw:  `null`,
			want: nil,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got StringList
			if err := json.Unmarshal([]byte(tt.raw), &got); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Unmarshal() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestStringListRejectsIncompatibleJSON(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{`42`, `{"value":"c000"}`, `["c000", 1]`} {
		var got StringList
		if err := json.Unmarshal([]byte(raw), &got); err == nil {
			t.Fatalf("Unmarshal(%s) error = nil, want rejection", raw)
		}
	}
}
