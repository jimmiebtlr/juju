package state

import (
	"container/list"
	"errors"
	"labix.org/v2/mgo"
	"launchpad.net/juju-core/state/api/params"
	"launchpad.net/juju-core/state/watcher"
	"launchpad.net/tomb"
	"reflect"
)

// StateWatcher watches any changes to the state.
// It's a stub type for the time being until allWatcher
// is complete.
type StateWatcher struct{}

func newStateWatcher(st *State) *StateWatcher {
	return &StateWatcher{}
}

// Stop stops the watcher.
func (w *StateWatcher) Stop() error {
	return nil
}

var StubNextDelta = []params.Delta{
	params.Delta{
		Removed: false,
		Entity: &params.ServiceInfo{
			Name:    "Example",
			Exposed: true,
		},
	},
	params.Delta{
		Removed: true,
		Entity: &params.UnitInfo{
			Name:    "MyUnit",
			Service: "Example",
		},
	},
}

// Next retrieves all changes that have happened since the given revision
// number, blocking until there are some changes available.  It also
// returns the revision number of the latest change.
func (w *StateWatcher) Next() ([]params.Delta, error) {
	// This is a stub to make progress with the higher level coding.
	return StubNextDelta, nil
}

// StateWatcher watches any changes to the state.
// TODO(rog) rename this to StateWatcher when allWatcher is complete.
type xStateWatcher struct {
	all *allWatcher
	// The following fields are maintained by the allWatcher
	// goroutine.
	revno   int64
	stopped bool
}

// Stop stops the watcher.
func (w *xStateWatcher) Stop() error {
	select {
	case w.all.request <- &allRequest{w: w}:
		return nil
	case <-w.all.tomb.Dead():
	}
	return w.all.tomb.Err()
}

var errWatcherStopped = errors.New("state watcher was stopped")

// Next retrieves all changes that have happened since the given revision
// number, blocking until there are some changes available.  It also
// returns the revision number of the latest change.
func (w *xStateWatcher) Next() ([]params.Delta, error) {
	req := &allRequest{
		w:     w,
		reply: make(chan bool),
	}
	select {
	case w.all.request <- req:
	case <-w.all.tomb.Dead():
		err := w.all.tomb.Err()
		if err == nil {
			err = errWatcherStopped
		}
		return nil, err
	}
	if ok := <-req.reply; !ok {
		// TODO better error?
		return nil, errWatcherStopped
	}
	return req.changes, nil
}

// allWatcher holds a shared record of all current state and replies to
// requests from StateWatches to tell them when it changes.
// TODO(rog) complete this type and its methods.
type allWatcher struct {
	tomb tomb.Tomb

	// backing knows how to fetch information from
	// the underlying state.
	backing allWatcherBacking

	// request receives requests from StateWatcher clients.
	request chan *allRequest

	// all holds information on everything the allWatcher cares about.
	all *allInfo

	// Each entry in the waiting map holds a linked list of Next requests
	// outstanding for the associated StateWatcher.
	waiting map[*xStateWatcher]*allRequest
}

// allWatcherBacking is the interface required
// by the allWatcher to access the underlying state.
// It is an interface for testing purposes.
// TODO(rog) complete this type and its methods.
type allWatcherBacking interface {
	// entityIdForInfo returns the entity id corresponding
	// to the given entity info.
	entityIdForInfo(info params.EntityInfo) entityId

	// getAll retrieves information about all known entities in the state
	// into the given allInfo.
	getAll(all *allInfo) error

	// fetch retrieves information about the entity with
	// the given id. It returns mgo.ErrNotFound if the
	// entity does not exist.
	fetch(id entityId) (params.EntityInfo, error)

	// watch watches for any changes and sends them
	// on the given channel.
	watch(in chan<- watcher.Change)

	// unwatch stops watching for changes on the
	// given channel.
	unwatch(in chan<- watcher.Change)
}

// entityId holds the mongo identifier of an entity.
type entityId struct {
	collection string
	id         interface{}
}

// allRequest holds a request from the StateWatcher to the
// allWatcher for some changes. The request will be
// replied to when some changes are available.
type allRequest struct {
	// w holds the StateWatcher that has originated the request.
	w *xStateWatcher

	// reply receives a message when deltas are ready.  If reply is
	// nil, the StateWatcher will be stopped.  If the reply is true,
	// the request has been processed; if false, the StateWatcher
	// has been stopped,
	reply chan bool

	// On reply, changes will hold changes that have occurred since
	// the last replied-to Next request.
	changes []params.Delta

	// next points to the next request in the list of outstanding
	// requests on a given watcher.  It is used only by the central
	// allWatcher goroutine.
	next *allRequest
}

// newAllWatcher returns a new allWatcher that retrieves information
// using the given backing. It does not start it running.
func newAllWatcher(backing allWatcherBacking) *allWatcher {
	return &allWatcher{
		backing: backing,
		request: make(chan *allRequest),
		all:     newAllInfo(),
		waiting: make(map[*xStateWatcher]*allRequest),
	}
}

func (aw *allWatcher) run() {
	defer aw.tomb.Done()
	// TODO(rog) distinguish between temporary and permanent errors:
	// if we get an error in loop, this logic kill the state's allWatcher
	// forever. This currently fits the way we go about things,
	// because we reconnect to the state on any error, but
	// perhaps there are errors we could recover from.
	aw.tomb.Kill(aw.loop())
}

func (aw *allWatcher) loop() error {
	in := make(chan watcher.Change)
	aw.backing.watch(in)
	defer aw.backing.unwatch(in)
	// We have no idea what changes the watcher might be trying to
	// send us while getAll proceeds, but we don't mind, because
	// allWatcher.changed is idempotent with respect to both updates
	// and removals.
	// TODO(rog) Perhaps find a way to avoid blocking all other
	// watchers while getAll is running.
	if err := aw.backing.getAll(aw.all); err != nil {
		return err
	}
	for {
		select {
		case <-aw.tomb.Dying():
			return tomb.ErrDying
		case change := <-in:
			id := entityId{
				collection: change.C,
				id:         change.Id,
			}
			if err := aw.changed(id); err != nil {
				return err
			}
		case req := <-aw.request:
			aw.handle(req)
		}
		aw.respond()
	}
	panic("unreachable")
}

// Stop stops the allWatcher.
func (aw *allWatcher) Stop() error {
	aw.tomb.Kill(nil)
	return aw.tomb.Wait()
}

// handle processes a request from a StateWatcher to the allWatcher.
func (aw *allWatcher) handle(req *allRequest) {
	if req.w.stopped {
		// The watcher has previously been stopped.
		if req.reply != nil {
			req.reply <- false
		}
		return
	}
	if req.reply == nil {
		// This is a request to stop the watcher.
		for req := aw.waiting[req.w]; req != nil; req = req.next {
			req.reply <- false
		}
		delete(aw.waiting, req.w)
		req.w.stopped = true
		aw.leave(req.w)
		return
	}
	// Add request to head of list.
	req.next = aw.waiting[req.w]
	aw.waiting[req.w] = req
}

// respond responds to all outstanding requests that are satisfiable.
func (aw *allWatcher) respond() {
	for w, req := range aw.waiting {
		revno := w.revno
		changes := aw.all.changesSince(revno)
		if len(changes) == 0 {
			continue
		}
		req.changes = changes
		w.revno = aw.all.latestRevno
		req.reply <- true
		if req := req.next; req == nil {
			// Last request for this watcher.
			delete(aw.waiting, w)
		} else {
			aw.waiting[w] = req
		}
		aw.seen(revno)
	}
}

// changed updates the allWatcher's idea of the current state
// in response to the given change.
func (aw *allWatcher) changed(id entityId) error {
	// TODO(rog) investigate ways that this can be made more efficient
	// than simply fetching each entity in turn.
	info, err := aw.backing.fetch(id)
	if err == mgo.ErrNotFound {
		aw.all.markRemoved(id)
		return nil
	}
	if err != nil {
		return err
	}
	aw.all.update(id, info)
	return nil
}

// seen states that a StateWatcher has just been given information about
// all entities newer than the given revno.  We assume it has already
// seen all the older entities.
func (aw *allWatcher) seen(revno int64) {
	for e := aw.all.list.Front(); e != nil; {
		next := e.Next()
		entry := e.Value.(*entityEntry)
		if entry.revno <= revno {
			break
		}
		if entry.creationRevno > revno {
			if !entry.removed {
				// This is a new entity that hasn't been seen yet,
				// so increment the entry's refCount.
				entry.refCount++
			}
		} else if entry.removed {
			// This is an entity that we previously saw, but
			// has now been removed, so decrement its refCount, removing
			// the entity if nothing else is waiting to be notified that it's
			// gone.
			aw.all.decRef(entry, aw.backing.entityIdForInfo(entry.info))
		}
		e = next
	}
}

// leave is called when the given watcher leaves.  It decrements the reference
// counts of any entities that have been seen by the watcher.
func (aw *allWatcher) leave(w *xStateWatcher) {
	for e := aw.all.list.Front(); e != nil; {
		next := e.Next()
		entry := e.Value.(*entityEntry)
		if entry.creationRevno <= w.revno {
			// The watcher has seen this entry.
			if entry.removed && entry.revno <= w.revno {
				// The entity has been removed and the
				// watcher has already been informed of that,
				// so its refcount has already been decremented.
				e = next
				continue
			}
			aw.all.decRef(entry, aw.backing.entityIdForInfo(entry.info))
		}
		e = next
	}
}

// entityEntry holds an entry in the linked list of all entities known
// to a StateWatcher.
type entityEntry struct {
	// The revno holds the local idea of the latest change to the
	// given entity.  It is not the same as the transaction revno -
	// this means we can unconditionally move a newly fetched entity
	// to the front of the list without worrying if the revno has
	// changed since the watcher reported it.
	revno int64

	// creationRevno holds the revision number when the
	// entity was created.
	creationRevno int64

	// removed marks whether the entity has been removed.
	removed bool

	// refCount holds a count of the number of watchers that
	// have seen this entity. When the entity is marked as removed,
	// the ref count is decremented whenever a StateWatcher that
	// has previously seen the entry now sees that it has been removed;
	// the entry will be deleted when all such StateWatchers have
	// been notified.
	refCount int

	// info holds the actual information on the entity.
	info params.EntityInfo
}

// allInfo holds a list of all entities known
// to a StateWatcher.
type allInfo struct {
	latestRevno int64
	entities    map[entityId]*list.Element
	list        *list.List
}

// newAllInfo returns an allInfo instance holding information about the
// current state of all entities in the environment.
func newAllInfo() *allInfo {
	all := &allInfo{
		entities: make(map[entityId]*list.Element),
		list:     list.New(),
	}
	return all
}

// add adds a new entity with the given id and associated
// information to the list.
func (a *allInfo) add(id entityId, info params.EntityInfo) {
	if a.entities[id] != nil {
		panic("adding new entry with duplicate id")
	}
	a.latestRevno++
	entry := &entityEntry{
		info:          info,
		revno:         a.latestRevno,
		creationRevno: a.latestRevno,
	}
	a.entities[id] = a.list.PushFront(entry)
}

// decRef decrements the reference count of an entry within the list,
// deleting it if it becomes zero and the entry is removed.
func (a *allInfo) decRef(entry *entityEntry, id entityId) {
	if entry.refCount--; entry.refCount > 0 {
		return
	}
	if entry.refCount < 0 {
		panic("negative reference count")
	}
	if !entry.removed {
		return
	}
	elem := a.entities[id]
	if elem == nil {
		panic("delete of non-existent entry")
	}
	delete(a.entities, id)
	a.list.Remove(elem)
}

// delete deletes the entry with the given entity id.
func (a *allInfo) delete(id entityId) {
	elem := a.entities[id]
	if elem == nil {
		return
	}
	delete(a.entities, id)
	a.list.Remove(elem)
}

// markRemoved marks that the entity with the given id has
// been removed from the state. If nothing has seen the
// entity, then we delete it immediately.
func (a *allInfo) markRemoved(id entityId) {
	if elem := a.entities[id]; elem != nil {
		entry := elem.Value.(*entityEntry)
		if entry.removed {
			return
		}
		a.latestRevno++
		if entry.refCount == 0 {
			a.delete(id)
			return
		}
		entry.revno = a.latestRevno
		entry.removed = true
		a.list.MoveToFront(elem)
	}
}

// update updates the information for the entity with
// the given id.
func (a *allInfo) update(id entityId, info params.EntityInfo) {
	elem := a.entities[id]
	if elem == nil {
		a.add(id, info)
		return
	}
	entry := elem.Value.(*entityEntry)
	// Nothing has changed, so change nothing.
	// TODO(rog) do the comparison more efficiently.
	if reflect.DeepEqual(info, entry.info) {
		return
	}
	// We already know about the entity; update its doc.
	a.latestRevno++
	entry.revno = a.latestRevno
	entry.info = info
	a.list.MoveToFront(elem)
}

// changesSince returns any changes that have occurred since
// the given revno, oldest first.
func (a *allInfo) changesSince(revno int64) []params.Delta {
	e := a.list.Front()
	n := 0
	for ; e != nil; e = e.Next() {
		entry := e.Value.(*entityEntry)
		if entry.revno <= revno {
			break
		}
		n++
	}
	if e != nil {
		// We've found an element that we've already seen.
		e = e.Prev()
	} else {
		// We haven't seen any elements, so we want all of them.
		e = a.list.Back()
		n++
	}
	changes := make([]params.Delta, 0, n)
	for ; e != nil; e = e.Prev() {
		entry := e.Value.(*entityEntry)
		if entry.removed && entry.creationRevno > revno {
			// Don't include entries that have been created
			// and removed since the revno.
			continue
		}
		changes = append(changes, params.Delta{
			Removed: entry.removed,
			Entity:  entry.info,
		})
	}
	return changes
}