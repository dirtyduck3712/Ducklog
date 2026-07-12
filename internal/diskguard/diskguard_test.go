package diskguard

import "testing"

func guardAt(u float64) *Guard {
	return New("/", func(string) (float64, error) { return u, nil })
}

func TestThresholds(t *testing.T) {
	cases := []struct {
		usage float64
		want  State
	}{
		{0.50, OK}, {0.80, Warn}, {0.85, Warn},
		{0.90, Reject}, {0.94, Reject}, {0.95, Purge}, {0.99, Purge},
	}
	for _, c := range cases {
		got, _, _ := guardAt(c.usage).State()
		if got != c.want {
			t.Fatalf("usage %.2f → %v; want %v", c.usage, got, c.want)
		}
	}
}
