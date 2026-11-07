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

// 旧的单次回应场景由替身明确选择结束，不再借生产消息上限终止无限 speak。
func testHasPublished(s *Store, room string) bool {
	var n int
	s.db.QueryRow("SELECT count(*) FROM messages WHERE conversation_id=? AND author_id NOT IN ('user','system')", room).Scan(&n)
	return n > 0
}
