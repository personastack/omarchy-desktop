package wirejson

import "testing"

func TestValidUniqueJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "object", raw: `{"id":1,"items":[{"name":"a"},{"name":"b"}]}`, want: true},
		{name: "scalar", raw: `true`, want: true},
		{name: "nested duplicate", raw: `{"outer":{"value":1,"value":2}}`, want: false},
		{name: "root duplicate", raw: `{"id":1,"id":2}`, want: false},
		{name: "trailing value", raw: `{} []`, want: false},
		{name: "invalid", raw: `{"id":}`, want: false},
		{name: "empty", raw: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ValidUniqueJSON([]byte(tc.raw)); got != tc.want {
				t.Fatalf("ValidUniqueJSON(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
