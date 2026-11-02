package kernel

import "testing"

func TestNormalizeDeviceIP(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1.2.3.4", "1.2.3.4"},
		{" 1.2.3.4 ", "1.2.3.4"},
		{"2001:db8:abcd:1234:5678:9abc:def0:1234", "2001:db8:abcd:1234::"},
		{"2001:db8:abcd:1234:ffff:ffff:ffff:ffff", "2001:db8:abcd:1234::"},
		{"2001:db8:abcd:1234::1", "2001:db8:abcd:1234::"},
		{"::1", "::"},
		{"::ffff:1.2.3.4", "1.2.3.4"},
		{"not-an-ip", "not-an-ip"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeDeviceIP(c.in); got != c.want {
			t.Errorf("NormalizeDeviceIP(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Two addresses in the same /64 must collapse to the same device key,
	// while addresses in different /64s must stay distinct.
	a := NormalizeDeviceIP("2001:db8:1:2:aaaa::1")
	b := NormalizeDeviceIP("2001:db8:1:2:bbbb::2")
	c := NormalizeDeviceIP("2001:db8:1:3::1")
	if a != b {
		t.Errorf("same /64 addresses gave different keys: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("different /64 addresses collapsed to the same key: %q", a)
	}
}
