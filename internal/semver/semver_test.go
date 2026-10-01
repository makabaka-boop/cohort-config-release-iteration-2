package semver

import "testing"

func TestParseAndCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.2", "1.2.0", 0},
		{"v2.0.1", "2.0.0", 1},
		{"1.4.2", "1.4.3", -1},
		{"10.0.0", "9.99.99", 1},
	}
	for _, c := range cases {
		va, err := Parse(c.a)
		if err != nil {
			t.Fatalf("parse %s: %v", c.a, err)
		}
		vb, _ := Parse(c.b)
		if got := Compare(va, vb); got != c.want {
			t.Errorf("Compare(%s,%s)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, s := range []string{"", "v1.2.3-rc1", "abc", "1.2.3.4"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) expected error", s)
		}
	}
}

func TestGTECompatibility(t *testing.T) {
	v100, _ := Parse("1.0.0")
	v142, _ := Parse("1.4.2")
	if !GTE(v142, v100) {
		t.Error("1.4.2 must satisfy 1.0.0")
	}
	if GTE(v100, v142) {
		t.Error("1.0.0 must not satisfy 1.4.2")
	}
}
