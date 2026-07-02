package scheduledchat

import (
	"testing"
	"time"
)

func TestNextRunAfterMonthlySkipsInvalidDates(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		day        int
		afterLocal time.Time
		wantLocal  time.Time
	}{
		{
			name:       "day 31 skips February",
			day:        31,
			afterLocal: time.Date(2026, time.January, 31, 9, 1, 0, 0, loc),
			wantLocal:  time.Date(2026, time.March, 31, 9, 0, 0, 0, loc),
		},
		{
			name:       "day 30 skips February",
			day:        30,
			afterLocal: time.Date(2026, time.January, 30, 9, 1, 0, 0, loc),
			wantLocal:  time.Date(2026, time.March, 30, 9, 0, 0, 0, loc),
		},
		{
			name:       "day 29 uses leap year February",
			day:        29,
			afterLocal: time.Date(2028, time.January, 29, 9, 1, 0, 0, loc),
			wantLocal:  time.Date(2028, time.February, 29, 9, 0, 0, 0, loc),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := &Task{
				ScheduleType: ScheduleTypeMonthly,
				Timezone:     "Asia/Shanghai",
				DayOfMonth:   tt.day,
				Hour:         9,
				Minute:       0,
			}
			got, err := NextRunAfter(task, tt.afterLocal.UTC())
			if err != nil {
				t.Fatalf("NextRunAfter returned error: %v", err)
			}
			if !got.Equal(tt.wantLocal.UTC()) {
				t.Fatalf("got %s, want %s", got.In(loc), tt.wantLocal)
			}
		})
	}
}

func TestSessionTitleForRunUsesExecutionDate(t *testing.T) {
	startedAt := time.Date(2026, time.June, 4, 16, 30, 0, 0, time.UTC)
	task := &Task{Timezone: "Asia/Shanghai", Name: "每日舆情"}
	run := &Run{
		ScheduledAt: time.Date(2026, time.June, 4, 15, 0, 0, 0, time.UTC),
		StartedAt:   &startedAt,
	}

	got := sessionTitleForRun(task, run)
	want := "2026.6.5 定时任务-每日舆情"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
