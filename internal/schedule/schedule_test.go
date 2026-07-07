package schedule

import (
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BlackDark/renovate-server/internal/config"
)

func TestAddPlatformValidatesTimezone(t *testing.T) {
	r := New(slog.New(slog.DiscardHandler))
	err := r.AddPlatform(config.Schedule{Crontabs: []string{"* * * * *"}, Timezone: "Mars/Olympus"}, func() {})
	if err == nil {
		t.Fatal("want timezone error")
	}
}

func TestAddPlatformValidatesCrontab(t *testing.T) {
	r := New(slog.New(slog.DiscardHandler))
	err := r.AddPlatform(config.Schedule{Crontabs: []string{"bogus"}}, func() {})
	if err == nil {
		t.Fatal("want crontab error")
	}
}

func TestScheduledJobFires(t *testing.T) {
	r := New(slog.New(slog.DiscardHandler))
	var fired atomic.Int32
	if err := r.AddPlatform(config.Schedule{Crontabs: []string{"@every 10ms"}}, func() {
		fired.Add(1)
	}); err != nil {
		t.Fatal(err)
	}
	r.Start()
	defer r.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for fired.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fired.Load() == 0 {
		t.Fatal("job never fired")
	}
}

func TestEmptyScheduleIsNoop(t *testing.T) {
	r := New(slog.New(slog.DiscardHandler))
	if err := r.AddPlatform(config.Schedule{}, func() {}); err != nil {
		t.Fatal(err)
	}
	r.Start()
	r.Stop()
}
