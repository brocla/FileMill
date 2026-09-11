package mailgun

import (
	"context"
	"strings"
	"testing"
)

// A panic in one delivery tick must not end the loop: the worker would keep
// running with no replies going out. The tick reports it and the next one
// runs normally.
func TestDeliveryTickSurvivesPanic(t *testing.T) {
	f, rep, _ := newAlertingFixture(t)
	f.addSubmission(t, 1, "excel@mill.test", "schedule.xlsx")
	f.engine.panicWith = "boom"

	f.service.deliverTick(context.Background())

	got := rep.only("panic")
	if len(got) != 1 {
		t.Fatalf("alerts = %+v, want one panic", rep.alerts)
	}
	for _, want := range []string{"boom", "goroutine"} {
		if !strings.Contains(got[0].Detail, want) {
			t.Errorf("panic detail lacks %q:\n%s", want, got[0].Detail)
		}
	}
	if !strings.Contains(f.logs.String(), "boom") {
		t.Errorf("the panic must be logged; got %q", f.logs.String())
	}

	f.engine.panicWith = nil
	f.service.deliverTick(context.Background())
	if !f.engine.delivered[1] {
		t.Error("the tick after a panic must deliver normally")
	}
}

func TestSweepTickSurvivesPanic(t *testing.T) {
	f, rep, _ := newAlertingFixture(t)
	f.engine.panicWith = "boom"

	f.service.sweepTick(context.Background())

	if got := rep.only("panic"); len(got) != 1 || !strings.Contains(got[0].Detail, "boom") {
		t.Fatalf("alerts = %+v, want one panic naming it", rep.alerts)
	}
}
