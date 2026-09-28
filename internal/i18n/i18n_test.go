package i18n

import (
	"testing"
	"time"
)

func TestCatalogsAreComplete(t *testing.T) {
	for key := range catalog[EN] {
		if _, ok := catalog[TW][key]; !ok {
			t.Errorf("zh-TW is missing %q", key)
		}
	}
	for key := range catalog[TW] {
		if _, ok := catalog[EN][key]; !ok {
			t.Errorf("zh-TW has %q which English lacks", key)
		}
	}
}

func TestFallbacks(t *testing.T) {
	if T("fr", "status.up") != "Operational" {
		t.Error("unknown language falls back to English")
	}
	if T(EN, "no.such.key") != "no.such.key" {
		t.Error("unknown key is shown as-is")
	}
}

func TestDuration(t *testing.T) {
	cases := []struct {
		d  time.Duration
		en string
		tw string
	}{
		{45 * time.Second, "45 seconds", "45 秒"},
		{time.Minute, "1 minute", "1 分鐘"},
		{2*time.Hour + 5*time.Minute + 30*time.Second, "2 hours 5 minutes", "2 小時 5 分鐘"},
		{3*24*time.Hour + 4*time.Hour, "3 days 4 hours", "3 天 4 小時"},
		{2*time.Hour + 30*time.Second, "2 hours", "2 小時"},
	}
	for _, c := range cases {
		if got := Duration(EN, c.d); got != c.en {
			t.Errorf("Duration(en, %s) = %q, want %q", c.d, got, c.en)
		}
		if got := Duration(TW, c.d); got != c.tw {
			t.Errorf("Duration(zh-TW, %s) = %q, want %q", c.d, got, c.tw)
		}
	}
}

func TestPercentNeverRoundsUpToFull(t *testing.T) {
	cases := map[float64]string{1: "100%", 0.99996: "99.99%", 0.9995: "99.95%", 0.973: "97.3%", 0.5: "50.0%"}
	for r, want := range cases {
		if got := Percent(r); got != want {
			t.Errorf("Percent(%v) = %q, want %q", r, got, want)
		}
	}
}

func TestRelative(t *testing.T) {
	if Ago(EN, 10*time.Second) != "just now" || Ago(EN, 3*time.Minute) != "3 minutes ago" || Ago(TW, 2*time.Hour) != "2 小時前" {
		t.Error("Ago")
	}
	if In(EN, 90*time.Second) != "in 1 minute" || In(TW, 3*24*time.Hour) != "3 天後" {
		t.Error("In")
	}
	if Short(450*time.Millisecond) != "450ms" || Short(1234*time.Millisecond) != "1.2s" || Short(3*time.Hour) != "3h" {
		t.Error("Short")
	}
}
