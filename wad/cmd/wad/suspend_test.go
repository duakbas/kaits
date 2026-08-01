package main

import (
	"testing"
	"time"
)

// The threshold has to ignore ordinary lateness and catch a real sleep. A false
// positive is worse than no detection at all: it sends someone hunting for a
// suspend that never happened.
func TestIsSuspendGap(t *testing.T) {
	tick := 30 * time.Second
	cases := []struct {
		name string
		gap  time.Duration
		want bool
	}{
		{"exactly on time", tick, false},
		{"slightly late", 31 * time.Second, false},
		{"loaded machine, doubly late", 60 * time.Second, false},
		{"very loaded, just under the line", 90 * time.Second, false},
		{"a couple of minutes", 2 * time.Minute, true},
		{"the reported case", 47 * time.Minute, true},
		{"overnight", 9 * time.Hour, true},
	}
	for _, tc := range cases {
		if got := isSuspendGap(tc.gap, tick); got != tc.want {
			t.Errorf("%s: isSuspendGap(%s) = %v, want %v", tc.name, tc.gap, got, tc.want)
		}
	}
}
