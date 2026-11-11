package cmd

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var durationPart = regexp.MustCompile(`^(\d+(?:\.\d+)?)(w|d|h|m|s)`)

// parseDuration parses durations such as "7d", "36h", "2w" or "1d12h". It
// extends time.ParseDuration with days and weeks, which are the natural
// units for an analysis window.
func parseDuration(s string) (time.Duration, error) {
	orig := s
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	units := map[string]time.Duration{"w": 7 * 24 * time.Hour, "d": 24 * time.Hour, "h": time.Hour, "m": time.Minute, "s": time.Second}
	var total time.Duration
	for s != "" {
		m := durationPart.FindStringSubmatch(s)
		if m == nil {
			return 0, fmt.Errorf("invalid duration %q (examples: 7d, 36h, 2w, 1d12h)", orig)
		}
		n, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", orig, err)
		}
		total += time.Duration(n * float64(units[m[2]]))
		s = s[len(m[0]):]
	}
	if total <= 0 {
		return 0, fmt.Errorf("duration %q must be positive", orig)
	}
	return total, nil
}

// formatWindow renders a duration compactly for display ("7d", "36h").
func formatWindow(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}

// parseFraction parses "0.2" or "20%" into 0.2.
func parseFraction(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "%") {
		v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, "%")), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid percentage %q", s)
		}
		return v / 100, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid fraction %q (use 0.2 or 20%%)", s)
	}
	if v > 5 {
		return 0, fmt.Errorf("ambiguous value %q: write %s%% for a percentage or %g for a fraction", s, s, v/100)
	}
	return v, nil
}
