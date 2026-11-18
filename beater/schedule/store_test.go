package schedule

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStoreActive(t *testing.T) {
	s := NewStore()
	s.Replace(map[string][]*BusRouting{
		"BUS1": {
			routing("NOW_PRI", "NOW_SEC", testNow.Add(-time.Hour), testNow.Add(time.Hour)),
			routing("NEXT_PRI", "", testNow.Add(time.Hour), testNow.Add(2*time.Hour)),
		},
		"OLD": {routing("P", "S", testNow.Add(-2*time.Hour), testNow.Add(-time.Hour))},
	}, testNow)

	if r, ok := s.Active("BUS1", testNow, true); !ok || r.Pri != "NOW_PRI" {
		t.Errorf("now = %+v %t", r, ok)
	}

	// the lookahead window becomes active once the current one ends
	if r, ok := s.Active("BUS1", testNow.Add(90*time.Minute), true); !ok || r.Pri != "NEXT_PRI" {
		t.Errorf("later = %+v %t", r, ok)
	}

	if _, ok := s.Active("OLD", testNow, true); ok {
		t.Error("expired routing was returned")
	}

	if r, ok := s.Active("OLD", testNow, false); !ok || r.Pri != "P" {
		t.Error("routing should be returned when end dates are not validated")
	}

	if _, ok := s.Active("MISSING", testNow, false); ok {
		t.Error("missing output was found")
	}
}

func TestStoreStartingWithin(t *testing.T) {
	s := NewStore()
	s.Replace(map[string][]*BusRouting{
		"BUS1": {
			routing("NOW_PRI", "", testNow.Add(-time.Hour), testNow.Add(4*time.Second)),
			routing("SOON_A", "", testNow.Add(3*time.Second), testNow.Add(time.Hour)),
			routing("SOON_B", "", testNow.Add(5*time.Second), testNow.Add(time.Hour)),
			routing("LATER", "", testNow.Add(6*time.Second), testNow.Add(time.Hour)),
		},
		"STARTED": {routing("P", "", testNow, testNow.Add(time.Hour))},
	}, testNow)

	got := s.StartingWithin("BUS1", testNow, 5*time.Second)
	if len(got) != 2 || got[0].Pri != "SOON_A" || got[1].Pri != "SOON_B" {
		t.Errorf("BUS1 = %+v, want SOON_A and SOON_B (the 5s boundary included), sorted", got)
	}

	// a window starting at t has already started, Active returns it
	if got := s.StartingWithin("STARTED", testNow, 5*time.Second); len(got) != 0 {
		t.Errorf("STARTED = %+v, want none", got)
	}

	if got := s.StartingWithin("BUS1", testNow, 0); len(got) != 0 {
		t.Errorf("0 tolerance = %+v, want none", got)
	}

	if got := s.StartingWithin("MISSING", testNow, 5*time.Second); len(got) != 0 {
		t.Errorf("MISSING = %+v, want none", got)
	}
}

// overlapping windows resolve to the one whose start is closest to now, then the shorter one
func TestStoreActiveOverlap(t *testing.T) {
	at := func(h, m int) time.Time {
		return testNow.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
	}

	tests := []struct {
		name   string
		routes []*BusRouting
		at     time.Time
		want   string
	}{
		{
			name: "shorter event inside a longer one",
			routes: []*BusRouting{
				routing("LONG", "LONG_B", at(0, 0), at(4, 0)),
				routing("SHORT", "SHORT_B", at(1, 0), at(2, 0)),
			},
			at:   at(1, 30),
			want: "SHORT",
		},
		{
			name: "longer event resumes after the shorter one ends",
			routes: []*BusRouting{
				routing("LONG", "", at(0, 0), at(4, 0)),
				routing("SHORT", "", at(1, 0), at(2, 0)),
			},
			at:   at(2, 30),
			want: "LONG",
		},
		{
			name: "before the shorter event starts",
			routes: []*BusRouting{
				routing("LONG", "", at(0, 0), at(4, 0)),
				routing("SHORT", "", at(1, 0), at(2, 0)),
			},
			at:   at(0, 30),
			want: "LONG",
		},
		{
			name: "start closest to now wins over the shorter window",
			routes: []*BusRouting{
				routing("EARLY_SHORT", "", at(0, 0), at(3, 0)),
				routing("LATE_LONG", "", at(1, 0), at(6, 0)),
			},
			at:   at(2, 0),
			want: "LATE_LONG",
		},
		{
			name: "same start resolves to the shorter window",
			routes: []*BusRouting{
				routing("LONG", "", at(0, 0), at(4, 0)),
				routing("SHORT", "", at(0, 0), at(1, 0)),
			},
			at:   at(0, 30),
			want: "SHORT",
		},
		{
			name: "back-to-back windows hand over at the boundary",
			routes: []*BusRouting{
				routing("FIRST", "", at(0, 0), at(1, 0)),
				routing("NEXT", "", at(1, 0), at(2, 0)),
			},
			at:   at(1, 0),
			want: "NEXT",
		},
		{
			name: "three nested windows",
			routes: []*BusRouting{
				routing("DAY", "", at(0, 0), at(12, 0)),
				routing("SHOW", "", at(2, 0), at(6, 0)),
				routing("INSERT", "", at(3, 0), at(3, 30)),
			},
			at:   at(3, 15),
			want: "INSERT",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewStore()
			s.Replace(map[string][]*BusRouting{"BUS1": tt.routes}, testNow)

			if r, ok := s.Active("BUS1", tt.at, true); !ok || r.Pri != tt.want {
				t.Errorf("active = %+v %t, want pri=%s", r, ok, tt.want)
			}
		})
	}
}

func TestStoreReplace(t *testing.T) {
	fetchStart := testNow
	active := func(pri string) []*BusRouting {
		return []*BusRouting{routing(pri, "", testNow.Add(-time.Hour), testNow.Add(time.Hour))}
	}

	s := NewStore()
	s.Replace(map[string][]*BusRouting{"BUS1": active("OLD1"), "BUS2": active("OLD2"), "GONE": active("G")}, fetchStart.Add(-5*time.Minute))

	// BUS2 is refreshed while the next poll is in flight
	s.ReplaceOutput("BUS2", active("REFRESHED"), fetchStart.Add(time.Second))

	kept := s.Replace(map[string][]*BusRouting{
		"BUS1":  active("POLL1"),
		"BUS2":  active("POLL2"),
		"NEW":   active("N"),
		"EMPTY": nil,
	}, fetchStart)

	if kept != 1 {
		t.Errorf("kept = %d, want 1", kept)
	}

	want := map[string]string{"BUS1": "POLL1", "BUS2": "REFRESHED", "NEW": "N"}
	for output, pri := range want {
		if r, ok := s.Active(output, testNow, true); !ok || r.Pri != pri {
			t.Errorf("%s = %+v %t, want pri=%s", output, r, ok, pri)
		}
	}

	if s.Length() != 3 {
		t.Errorf("length = %d, want 3 (GONE and EMPTY removed)", s.Length())
	}
}

func TestStoreReplaceOutput(t *testing.T) {
	start, end := testNow.Add(-time.Hour), testNow.Add(time.Hour)
	next := routing("NEXT", "", end, end.Add(time.Hour))

	seed := func() *Store {
		s := NewStore()
		s.Replace(map[string][]*BusRouting{
			"BUS1":  {routing("PRI", "SEC", start, end), next},
			"OTHER": {routing("O", "", start, end)},
		}, testNow.Add(-time.Minute))
		return s
	}

	t.Run("refreshed windows replace the stored ones", func(t *testing.T) {
		s := seed()
		held, _ := s.Active("BUS1", testNow, true)

		if !s.ReplaceOutput("BUS1", []*BusRouting{routing("NEW_PRI", "", start, end)}, testNow) {
			t.Fatal("not applied")
		}

		// a MAIN only refresh is the whole schedule for the bus, the old backup isn't kept
		got := s.Routes("BUS1")
		if len(got) != 1 || got[0].Pri != "NEW_PRI" || got[0].Sec != "" {
			t.Errorf("routes = %+v, want only the refreshed window", got)
		}

		if held.Pri != "PRI" {
			t.Error("routing held by a reader was mutated in place")
		}

		if r, ok := s.Active("OTHER", testNow, true); !ok || r.Pri != "O" {
			t.Errorf("OTHER = %+v %t, want untouched", r, ok)
		}
	})

	t.Run("overlapping events stay separate", func(t *testing.T) {
		s := seed()

		s.ReplaceOutput("BUS1", []*BusRouting{
			routing("SHORT", "SHORT_B", testNow.Add(-10*time.Minute), testNow.Add(10*time.Minute)),
			routing("LONG", "LONG_B", start, end.Add(2*time.Hour)),
		}, testNow)

		got := s.Routes("BUS1")
		if len(got) != 2 || got[0].Pri != "LONG" || got[1].Pri != "SHORT" {
			t.Fatalf("routes = %+v, want both windows sorted by start", got)
		}

		if r, _ := s.Active("BUS1", testNow, true); r.Pri != "SHORT" || r.Sec != "SHORT_B" {
			t.Errorf("active = %+v, want the shorter event", r)
		}
	})

	t.Run("new output", func(t *testing.T) {
		s := seed()

		s.ReplaceOutput("BUS2", []*BusRouting{routing("B", "", start, end)}, testNow)

		if r, ok := s.Active("BUS2", testNow, true); !ok || r.Pri != "B" {
			t.Errorf("BUS2 = %+v %t", r, ok)
		}
	})

	t.Run("refresh is kept by a later poll", func(t *testing.T) {
		s := seed()

		s.ReplaceOutput("BUS1", []*BusRouting{routing("REFRESHED", "", start, end)}, testNow)

		if kept := s.Replace(map[string][]*BusRouting{"BUS1": {routing("POLL", "", start, end)}}, testNow.Add(-time.Second)); kept != 1 {
			t.Errorf("kept = %d, want 1", kept)
		}

		if r, _ := s.Active("BUS1", testNow, true); r.Pri != "REFRESHED" {
			t.Errorf("active = %+v, want the refresh newer than the poll", r)
		}
	})

	t.Run("no current window keeps the stored entry", func(t *testing.T) {
		s := seed()

		applied := s.ReplaceOutput("BUS1", []*BusRouting{
			routing("PAST", "", start.Add(-2*time.Hour), start.Add(-time.Hour)),
			{Pri: "NO_WINDOW"},
			nil,
		}, testNow)

		if applied {
			t.Error("nothing should be applied")
		}

		if got := s.Routes("BUS1"); len(got) != 2 || got[0].Pri != "PRI" {
			t.Errorf("routes = %+v, want unchanged", got)
		}

		if s.ReplaceOutput("BUS1", nil, testNow) || len(s.Routes("BUS1")) != 2 {
			t.Error("an empty refresh should keep the polled windows")
		}

		if s.ReplaceOutput("BUS9", nil, testNow) || s.Length() != 2 {
			t.Error("an empty refresh should not create an entry")
		}
	})

	t.Run("caller's windows are copied", func(t *testing.T) {
		s := seed()
		w := routing("MINE", "", start, end)

		s.ReplaceOutput("BUS1", []*BusRouting{w}, testNow)
		w.Pri = "CHANGED"

		if r, _ := s.Active("BUS1", testNow, true); r.Pri != "MINE" {
			t.Errorf("active = %+v, the stored window changed with the caller's", r)
		}
	})
}

func TestStorePrune(t *testing.T) {
	seed := func() *Store {
		s := NewStore()
		s.Replace(map[string][]*BusRouting{
			"ACTIVE": {routing("A", "", testNow.Add(-time.Hour), testNow.Add(time.Hour))},
			"FUTURE": {routing("F", "", testNow.Add(time.Hour), testNow.Add(2*time.Hour))},
			"MIXED": {
				routing("OLD", "", testNow.Add(-2*time.Hour), testNow.Add(-time.Second)),
				routing("CUR", "", testNow.Add(-time.Second), testNow.Add(time.Hour)),
			},
			"EXPIRED": {routing("E", "", testNow.Add(-2*time.Hour), testNow.Add(-time.Second))},
			"NODATES": {{Pri: "N"}},
		}, testNow)
		return s
	}

	check := func(t *testing.T, s *Store) {
		t.Helper()

		windows, outputs := s.Prune(testNow)
		if windows != 3 || outputs != 2 {
			t.Errorf("removed windows=%d outputs=%d, want 3 and 2", windows, outputs)
		}

		for _, key := range []string{"ACTIVE", "FUTURE", "MIXED"} {
			if len(s.Routes(key)) != 1 {
				t.Errorf("%s = %+v, want one window kept", key, s.Routes(key))
			}
		}

		if r := s.Routes("MIXED"); len(r) == 1 && r[0].Pri != "CUR" {
			t.Errorf("MIXED kept %+v, want CUR", r[0])
		}

		if s.Length() != 3 {
			t.Errorf("length = %d, want 3", s.Length())
		}
	}

	t.Run("in memory", func(t *testing.T) {
		check(t, seed())
	})

	t.Run("after save and load", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "schedule.gob")

		if err := seed().SaveToFile(file); err != nil {
			t.Fatal(err)
		}

		s := NewStore()
		if err := s.LoadFromFile(file); err != nil {
			t.Fatal(err)
		}

		check(t, s)
	})
}

func TestStoreConcurrentAccess(t *testing.T) {
	s := NewStore()
	start, end := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)

	var wg sync.WaitGroup

	for i := range 50 {
		wg.Add(4)

		go func() {
			defer wg.Done()
			s.ReplaceOutput("BUS1", []*BusRouting{routing(fmt.Sprintf("SRC%d", i), "", start, end)}, time.Now())
		}()

		go func() {
			defer wg.Done()
			s.Replace(map[string][]*BusRouting{"BUS1": {routing("POLL", "", start, end)}}, time.Now())
		}()

		go func() {
			defer wg.Done()
			s.Prune(time.Now())
		}()

		go func() {
			defer wg.Done()
			if r, ok := s.Active("BUS1", time.Now(), true); ok {
				_ = r.Pri + r.Sec
			}
		}()
	}

	wg.Wait()
}
