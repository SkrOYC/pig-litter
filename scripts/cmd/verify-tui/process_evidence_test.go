package main

import "testing"

func TestProcessIdentityUsesStartTimeAfterParenthesizedName(t *testing.T) {
	const stat = "123 (worker (with spaces)) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 987654 20"
	start, err := procStartTicks(stat)
	if err != nil || start != "987654" {
		t.Fatalf("start ticks = %q %v", start, err)
	}
	for _, bad := range []string{"", "123 worker S", "123 (worker) S 1 2"} {
		if _, err := procStartTicks(bad); err == nil {
			t.Fatalf("accepted invalid process stat %q", bad)
		}
	}
}
