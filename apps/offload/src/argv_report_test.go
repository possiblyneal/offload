package main

import (
	"strings"
	"testing"
	"time"
)

func TestEmptyArgumentsSurviveInTheLauncherCommand(t *testing.T) {
	got := launcherCommand(job{devAddr: "192.0.2.50", argv: []string{"printf", "[%s]", "a", "", "b"}})
	if want := ` -- 'printf' '[%s]' 'a' '' 'b'`; !strings.HasSuffix(got, want) {
		t.Errorf("launcherCommand = %q, want it to end %q", got, want)
	}
}

func TestAReportRecordOverAMegabyteDoesNotHideTheOnesAfterIt(t *testing.T) {
	recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	long := `{"time":"` + recent + `","placement":"local","rule":"sudo","exit_code":0,"argv":["` + strings.Repeat("x", 2<<20) + `"]}`
	short := `{"time":"` + recent + `","placement":"server","rule":"free-slot","exit_code":0}`
	recs := readRecords(strings.NewReader(long + "\n" + short + "\n"))
	if len(recs) != 2 {
		t.Fatalf("read %d records, want 2", len(recs))
	}
}
