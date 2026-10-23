package core

import (
	"testing"
	"time"
)

func TestUnder13(t *testing.T) {
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	for bday, want := range map[string]bool{
		"2020-01-01": true, "2013-10-24": true, "2013-10-23": false,
		"1990-01-01": false, "": false, "garbage": false,
	} {
		if got := Under13(bday, now); got != want {
			t.Errorf("Under13(%q) = %v, want %v", bday, got, want)
		}
	}
}
