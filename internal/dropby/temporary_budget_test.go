package dropby

import (
	"testing"
	"time"
)

// 本周开发调额不能永久扩大预算，也不能依赖下周有人手动重启。
func TestTemporaryBudgetExpires(t *testing.T) {
	until := time.Date(2026, 11, 9, 0, 0, 0, 0, time.FixedZone("CST", 8*3600))
	c := Config{WeeklyBudgetMicros: 20_000_000, TemporaryBudgetMicros: 30_000_000, TemporaryBudgetUntil: until}
	for _, tc := range []struct {
		at   time.Time
		want int64
	}{
		{until.Add(-time.Second), 30_000_000}, {until, 20_000_000}, {until.Add(time.Hour), 20_000_000},
	} {
		if got := c.weeklyBudgetAt(tc.at); got != tc.want {
			t.Fatalf("at %v: got %d want %d", tc.at, got, tc.want)
		}
	}
}
