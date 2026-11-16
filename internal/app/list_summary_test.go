package app

import "testing"

func TestPortableScheduleSummary(t *testing.T) {
	got := portableScheduleSummary("0 4 * * *", "Asia/Shanghai", "5min", "30min", "2min")
	want := "0 4 * * * Asia/Shanghai, boot+5min, every 30min, jitter 2min"
	if got != want {
		t.Fatalf("portableScheduleSummary() = %q, want %q", got, want)
	}
}
