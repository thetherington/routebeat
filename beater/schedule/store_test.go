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

func TestStoreReplace(t *testing.T) {
	fetchStart := testNow
	active := func(pri string) []*BusRouting {
		return []*BusRouting{routing(pri, "", testNow.Add(-time.Hour), testNow.Add(time.Hour))}
	}

	s := NewStore()
	s.Replace(map[string][]*BusRouting{"BUS1": active("OLD1"), "BUS2": active("OLD2"), "GONE": active("G")}, fetchStart.Add(-5*time.Minute))

	// BUS2 is refreshed while the next poll is in flight
	s.Upsert("BUS2", active("REFRESHED"), fetchStart.Add(time.Second))

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

func TestStoreUpsert(t *testing.T) {
	start, end := testNow.Add(-time.Hour), testNow.Add(time.Hour)
	next := routing("NEXT", "", end, end.Add(time.Hour))

	seed := func() *Store {
		s := NewStore()
		s.Replace(map[string][]*BusRouting{"BUS1": {routing("PRI", "SEC", start, end), next}}, testNow.Add(-time.Minute))
		return s
	}

	t.Run("identical window keeps the other source", func(t *testing.T) {
		s := seed()
		held, _ := s.Active("BUS1", testNow, true)

		if !s.Upsert("BUS1", []*BusRouting{routing("NEW_PRI", "", start, end)}, testNow) {
			t.Fatal("not applied")
		}

		r, _ := s.Active("BUS1", testNow, true)
		if r.Pri != "NEW_PRI" || r.Sec != "SEC" {
			t.Errorf("got %+v, want pri=NEW_PRI sec=SEC", r)
		}

		if held.Pri != "PRI" {
			t.Error("routing held by a reader was mutated in place")
		}

		if got := s.Routes("BUS1"); len(got) != 2 || got[1] != next {
			t.Errorf("routes = %+v, want the next window untouched", got)
		}
	})

	t.Run("advanced window merges over the overlapping one", func(t *testing.T) {
		s := seed()

		s.Upsert("BUS1", []*BusRouting{routing("", "ADV_SEC", start.Add(-10*time.Minute), end)}, testNow)

		got := s.Routes("BUS1")
		if len(got) != 2 || got[0].Pri != "PRI" || got[0].Sec != "ADV_SEC" || !got[0].StartDate.Equal(start.Add(-10*time.Minute)) {
			t.Errorf("routes = %+v", got)
		}
	})

	t.Run("separate window is inserted in order", func(t *testing.T) {
		s := seed()

		s.Upsert("BUS1", []*BusRouting{routing("LATER", "", end.Add(2*time.Hour), end.Add(3*time.Hour))}, testNow)

		got := s.Routes("BUS1")
		if len(got) != 3 || got[2].Pri != "LATER" {
			t.Errorf("routes = %+v", got)
		}
	})

	t.Run("new output", func(t *testing.T) {
		s := seed()

		s.Upsert("BUS2", []*BusRouting{routing("B", "", start, end)}, testNow)

		if r, ok := s.Active("BUS2", testNow, true); !ok || r.Pri != "B" {
			t.Errorf("BUS2 = %+v %t", r, ok)
		}
	})

	t.Run("expired and dateless windows are not applied", func(t *testing.T) {
		s := seed()

		applied := s.Upsert("BUS1", []*BusRouting{
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

		if s.Upsert("BUS9", nil, testNow) || s.Length() != 1 {
			t.Error("an empty upsert should not create an entry")
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
			s.Upsert("BUS1", []*BusRouting{routing(fmt.Sprintf("SRC%d", i), "", start, end)}, time.Now())
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
