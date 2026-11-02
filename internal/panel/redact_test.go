package panel

import (
	"strings"
	"testing"
)

func TestRedactURL(t *testing.T) {
	const secret = "super-secret-token"
	cases := []struct {
		name  string
		in    string
		want  string
		notes string
	}{
		{
			name: "token replaced, other params preserved",
			in:   "ws://panel.example.com:8076?token=" + secret + "&node_id=1",
			want: "ws://panel.example.com:8076?node_id=1&token=REDACTED",
		},
		{
			name: "machine id preserved",
			in:   "ws://panel.example.com:8076?machine_id=7&token=" + secret,
			want: "ws://panel.example.com:8076?machine_id=7&token=REDACTED",
		},
		{
			name: "no token left unchanged",
			in:   "https://panel.example.com/api/v2/server/user?node_id=1",
			want: "https://panel.example.com/api/v2/server/user?node_id=1",
		},
		{
			name: "no query left unchanged",
			in:   "https://panel.example.com/api/v2/server/user",
			want: "https://panel.example.com/api/v2/server/user",
		},
		{
			name: "unparsable input is never printed verbatim",
			in:   "://bad url?token=" + secret,
			want: "[unparsable url]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactURL(tc.in)
			if got != tc.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, secret) {
				t.Errorf("RedactURL output still contains the token: %q", got)
			}
		})
	}
}
