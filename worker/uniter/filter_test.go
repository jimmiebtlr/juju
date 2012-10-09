package uniter

import (
	. "launchpad.net/gocheck"
	"launchpad.net/juju-core/charm"
	"launchpad.net/juju-core/juju/testing"
	"launchpad.net/juju-core/state"
	"launchpad.net/juju-core/worker"
	"time"
)

type FilterSuite struct {
	testing.JujuConnSuite
	ch   *state.Charm
	svc  *state.Service
	unit *state.Unit
}

var _ = Suite(&FilterSuite{})

func (s *FilterSuite) SetUpTest(c *C) {
	s.JujuConnSuite.SetUpTest(c)
	s.ch = s.AddTestingCharm(c, "dummy")
	var err error
	s.svc, err = s.State.AddService("dummy", s.ch)
	c.Assert(err, IsNil)
	s.unit, err = s.svc.AddUnit()
	c.Assert(err, IsNil)
}

func (s *FilterSuite) TestUnitDeath(c *C) {
	f, err := newFilter(s.State, s.unit.Name())
	c.Assert(err, IsNil)
	defer f.Stop()
	assertNotClosed := func() {
		s.State.StartSync()
		select {
		case <-time.After(50 * time.Millisecond):
		case <-f.UnitDying():
			c.Fatalf("unexpected receive")
		}
	}
	assertNotClosed()

	// Irrelevant change.
	err = s.unit.SetResolved(state.ResolvedRetryHooks)
	c.Assert(err, IsNil)
	assertNotClosed()

	// Set dying.
	err = s.unit.EnsureDying()
	c.Assert(err, IsNil)
	assertClosed := func() {
		s.State.StartSync()
		select {
		case <-time.After(50 * time.Millisecond):
			c.Fatalf("dying not detected")
		case _, ok := <-f.UnitDying():
			c.Assert(ok, Equals, false)
		}
	}
	assertClosed()

	// Another irrelevant change.
	err = s.unit.ClearResolved()
	c.Assert(err, IsNil)
	assertClosed()

	// Set dead.
	err = s.unit.EnsureDead()
	c.Assert(err, IsNil)
	s.State.StartSync()
	select {
	case <-f.Dying():
	case <-time.After(50 * time.Millisecond):
		c.Fatalf("dead not detected")
	}
	c.Assert(f.Wait(), Equals, worker.ErrDead)
}

func (s *FilterSuite) TestServiceDeath(c *C) {
	f, err := newFilter(s.State, s.unit.Name())
	c.Assert(err, IsNil)
	defer f.Stop()
	s.State.StartSync()
	select {
	case <-time.After(50 * time.Millisecond):
	case <-f.UnitDying():
		c.Fatalf("unexpected receive")
	}

	err = s.svc.EnsureDying()
	c.Assert(err, IsNil)
	timeout := time.After(500 * time.Millisecond)
loop:
	for {
		select {
		case <-f.UnitDying():
			break loop
		case <-time.After(50 * time.Millisecond):
			s.State.StartSync()
		case <-timeout:
			c.Fatalf("dead not detected")
		}
	}
	err = s.unit.Refresh()
	c.Assert(err, IsNil)
	c.Assert(s.unit.Life(), Equals, state.Dying)

	// Can't set s.svc to Dead while it still has units.
}

func (s *FilterSuite) TestResolvedEvents(c *C) {
	f, err := newFilter(s.State, s.unit.Name())
	c.Assert(err, IsNil)
	defer f.Stop()

	// No initial event; not worth mentioning ResolvedNone.
	assertNoChange := func() {
		s.State.StartSync()
		select {
		case rm := <-f.ResolvedEvents():
			c.Fatalf("unexpected %#v", rm)
		case <-time.After(50 * time.Millisecond):
		}
	}
	assertNoChange()

	// Request an event; no interesting event is available.
	f.WantResolvedEvent()
	assertNoChange()

	// Change the unit in an irrelevant way; no events.
	err = s.unit.SetCharm(s.ch)
	c.Assert(err, IsNil)
	assertNoChange()

	// Change the unit's resolved to an interesting value; new event received.
	err = s.unit.SetResolved(state.ResolvedRetryHooks)
	c.Assert(err, IsNil)
	assertChange := func(expect state.ResolvedMode) {
		s.State.Sync()
		select {
		case rm := <-f.ResolvedEvents():
			c.Assert(rm, Equals, expect)
		case <-time.After(50 * time.Millisecond):
			c.Fatalf("timed out")
		}
	}
	assertChange(state.ResolvedRetryHooks)
	assertNoChange()

	// Request a few events, and change the unit a few times; when
	// we finally receive, only the most recent state is sent.
	f.WantResolvedEvent()
	err = s.unit.ClearResolved()
	c.Assert(err, IsNil)
	f.WantResolvedEvent()
	err = s.unit.SetResolved(state.ResolvedNoHooks)
	c.Assert(err, IsNil)
	f.WantResolvedEvent()
	assertChange(state.ResolvedNoHooks)
	assertNoChange()
}

func (s *FilterSuite) TestCharmEvents(c *C) {
	f, err := newFilter(s.State, s.unit.Name())
	c.Assert(err, IsNil)
	defer f.Stop()

	// No initial event is sent.
	assertNoChange := func() {
		s.State.StartSync()
		select {
		case sch := <-f.UpgradeEvents():
			c.Fatalf("unexpected %#v", sch)
		case <-time.After(50 * time.Millisecond):
		}
	}
	assertNoChange()

	// Request an event relative to the existing state; nothing.
	f.WantUpgradeEvent(s.ch.URL(), false)
	assertNoChange()

	// Change the service in an irrelevant way; no events.
	err = s.svc.SetExposed()
	c.Assert(err, IsNil)
	assertNoChange()

	// Change the service's charm; new event received.
	ch := s.AddTestingCharm(c, "dummy-v2")
	err = s.svc.SetCharm(ch, false)
	c.Assert(err, IsNil)
	assertChange := func(url *charm.URL) {
		s.State.Sync()
		select {
		case sch := <-f.UpgradeEvents():
			c.Assert(sch.URL(), DeepEquals, url)
		case <-time.After(50 * time.Millisecond):
			c.Fatalf("timed out")
		}
	}
	assertChange(ch.URL())
	assertNoChange()

	// Request a change relative to the original state, unforced;
	// same event is sent.
	f.WantUpgradeEvent(s.ch.URL(), false)
	assertChange(ch.URL())
	assertNoChange()

	// Request a forced change relative to the initial state; no change...
	f.WantUpgradeEvent(s.ch.URL(), true)
	assertNoChange()

	// ...and still no change when we have a forced upgrade to that state...
	err = s.svc.SetCharm(s.ch, true)
	c.Assert(err, IsNil)
	assertNoChange()

	// ...but a *forced* change to a different charm does generate an event.
	err = s.svc.SetCharm(ch, true)
	assertChange(ch.URL())
	assertNoChange()
}

func (s *FilterSuite) TestConfigEvents(c *C) {
	f, err := newFilter(s.State, s.unit.Name())
	c.Assert(err, IsNil)
	defer f.Stop()

	// Initial event.
	assertChange := func() {
		s.State.Sync()
		select {
		case _, ok := <-f.ConfigEvents():
			c.Assert(ok, Equals, true)
		case <-time.After(50 * time.Millisecond):
			c.Fatalf("timed out")
		}
	}
	assertChange()
	assertNoChange := func() {
		s.State.StartSync()
		select {
		case <-f.ConfigEvents():
			c.Fatalf("unexpected config event")
		case <-time.After(50 * time.Millisecond):
		}
	}
	assertNoChange()

	// Request an event; it matches the previous one.
	f.WantConfigEvent()
	assertChange()
	assertNoChange()

	// Change the config; new event received.
	node, err := s.svc.Config()
	c.Assert(err, IsNil)
	node.Set("skill-level", 9001)
	_, err = node.Write()
	c.Assert(err, IsNil)
	assertChange()
	assertNoChange()

	// Request a few events, and change the config a few times; when
	// we finally receive, only a single event is sent.
	f.WantConfigEvent()
	node.Set("title", "20,000 leagues in the cloud")
	_, err = node.Write()
	c.Assert(err, IsNil)
	f.WantConfigEvent()
	node.Set("outlook", "precipitous")
	_, err = node.Write()
	c.Assert(err, IsNil)
	f.WantConfigEvent()
	assertChange()
	assertNoChange()
}