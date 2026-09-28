package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that also accepts day and week units
// ("7d", "2w", "1d12h") in configuration files.
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return FormatDuration(time.Duration(d)) }

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	// Returning *yaml.TypeError lets the decoder keep going and report
	// every bad value in the file, not just the first one.
	if n.Kind != yaml.ScalarNode {
		return &yaml.TypeError{Errors: []string{fmt.Sprintf("line %d: expected a duration such as 30s, 5m or 7d", n.Line)}}
	}
	v, err := ParseDuration(n.Value)
	if err != nil {
		return &yaml.TypeError{Errors: []string{fmt.Sprintf("line %d: %v", n.Line, err)}}
	}
	*d = Duration(v)
	return nil
}

// ParseDuration parses Go duration syntax extended with "d" (24h) and
// "w" (7d). A bare "0" is accepted.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if s == "0" {
		return 0, nil
	}
	var total time.Duration
	rest := s
	for rest != "" {
		i := 0
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
			i++
		}
		if i == 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		num := rest[:i]
		j := i
		for j < len(rest) && (rest[j] < '0' || rest[j] > '9') && rest[j] != '.' {
			j++
		}
		unit := rest[i:j]
		rest = rest[j:]
		var mult time.Duration
		switch unit {
		case "w":
			mult = 7 * 24 * time.Hour
		case "d":
			mult = 24 * time.Hour
		case "h":
			mult = time.Hour
		case "m":
			mult = time.Minute
		case "s":
			mult = time.Second
		case "ms":
			mult = time.Millisecond
		case "":
			return 0, fmt.Errorf("invalid duration %q: missing unit (use s, m, h or d)", s)
		default:
			return 0, fmt.Errorf("invalid duration %q: unknown unit %q", s, unit)
		}
		f, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		total += time.Duration(f * float64(mult))
	}
	return total, nil
}

// FormatDuration renders a duration compactly, using days for long spans:
// 90s -> "1m30s", 36h -> "1d12h", 0 -> "0s".
func FormatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	var b strings.Builder
	if d < 0 {
		b.WriteByte('-')
		d = -d
	}
	if d < time.Second {
		return b.String() + d.String()
	}
	units := []struct {
		n string
		d time.Duration
	}{{"d", 24 * time.Hour}, {"h", time.Hour}, {"m", time.Minute}, {"s", time.Second}}
	for _, u := range units {
		if d >= u.d {
			fmt.Fprintf(&b, "%d%s", d/u.d, u.n)
			d %= u.d
		}
	}
	return b.String()
}
