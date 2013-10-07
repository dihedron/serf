package serf

import (
	"github.com/hashicorp/memberlist"
	"time"
)

type statusChange struct {
	member    *Member
	oldStatus MemberStatus
	newStatus MemberStatus
}

// changeHandler is a long running routine to coalesce updates,
// and apply a partition detection heuristic
func (s *Serf) changeHandler() {
	// Run until indicated otherwise
	for s.coalesceUpdates() {
	}
}

// coalesceUpdates will collect all the changes we receive until we
// either reach a quiescent period, or we reach the maximum coaslecing time.
// The coalesced updates are then forwarded to the Delegate
func (s *Serf) coalesceUpdates() bool {
	initialStatus := make(map[*Member]MemberStatus)
	endStatus := make(map[*Member]MemberStatus)
	var coalesceDone <-chan time.Time
	var quiescent <-chan time.Time

OUTER:
	for {
		select {
		case c := <-s.changeCh:
			// Mark the initial and end status of the member
			if _, ok := initialStatus[c.member]; !ok {
				initialStatus[c.member] = c.oldStatus
			}
			endStatus[c.member] = c.newStatus

			// Setup an end timer if none exists
			if coalesceDone == nil {
				coalesceDone = time.After(s.conf.MaxCoalesceTime)
			}

			// Setup a new quiescent timer
			quiescent = time.After(s.conf.MinQuiescentTime)

		case <-coalesceDone:
			break OUTER
		case <-quiescent:
			break OUTER
		case <-s.shutdownCh:
			return false
		}
	}

	// Fire any relevant events
	s.invokeDelegate(initialStatus, endStatus)
	return true
}

// partitionedNodes into various groups based on their start and end states
func partitionEvents(initial, end map[*Member]MemberStatus) (joined, left, failed, partitioned []*Member) {
	for member, endState := range end {
		initState := initial[member]

		// If a node is flapping, ignore it
		if endState == initState {
			continue
		}

		switch endState {
		case StatusAlive:
			joined = append(joined, member)
		case StatusLeft:
			left = append(left, member)
		case StatusFailed:
			failed = append(failed, member)
		case StatusPartitioned:
			partitioned = append(partitioned, member)
		}
	}
	return
}

// invokeDelegate is called to invoke the various delegate events
// after the updates have been coalesced
func (s *Serf) invokeDelegate(initial, end map[*Member]MemberStatus) {
	// Bail if no delegate
	d := s.conf.Delegate
	if d == nil {
		return
	}

	// Partition the nodes
	joined, left, failed, partitioned := partitionEvents(initial, end)

	// Invoke appropriate callbacks
	if len(joined) > 0 {
		d.MembersJoined(joined)
	}
	if len(left) > 0 {
		d.MembersLeft(left)
	}
	if len(failed) > 0 {
		d.MembersFailed(failed)
	}
	if len(partitioned) > 0 {
		d.MembersPartitioned(partitioned)
	}
}

// NotifyJoin is fired when memberlist detects a node join
func (s *Serf) NotifyJoin(n *memberlist.Node) {
	s.memberLock.Lock()
	defer s.memberLock.Unlock()

	// Check if we know about this node already
	mem, ok := s.members[n.Name]
	oldStatus := StatusNone
	if !ok {
		mem = &Member{
			Name:   n.Name,
			Addr:   n.Addr,
			Role:   string(n.Meta),
			Status: StatusAlive,
		}
		s.members[n.Name] = mem
	} else {
		oldStatus = mem.Status
		mem.Status = StatusAlive
	}

	// Notify about change
	s.changeCh <- statusChange{mem, oldStatus, StatusAlive}

	// Check if node was previously in a failed state
	if oldStatus != StatusFailed && oldStatus != StatusPartitioned {
		return
	}

	// Unsuspect a partition
	s.unsuspectPartition(mem)

	// Remove from failed or left lists
	s.failedMembers = removeOldMember(s.failedMembers, mem)
	s.leftMembers = removeOldMember(s.leftMembers, mem)
}

// NotifyLeave is fired when memberlist detects a node leave
func (s *Serf) NotifyLeave(n *memberlist.Node) {
	s.memberLock.Lock()
	defer s.memberLock.Unlock()

	// Check if we know about this node
	mem, ok := s.members[n.Name]
	if !ok {
		return
	}

	// Determine the state change
	oldStatus := mem.Status
	switch mem.Status {
	case StatusAlive:
		mem.Status = StatusFailed
		s.failedMembers = append(s.failedMembers, &oldMember{member: mem, time: time.Now()})

	case StatusLeaving:
		mem.Status = StatusLeft
		s.leftMembers = append(s.leftMembers, &oldMember{member: mem, time: time.Now()})
	}

	// Check if we should notify about a change
	s.changeCh <- statusChange{mem, oldStatus, mem.Status}

	// Suspect a partition on failure
	if mem.Status == StatusFailed {
		s.suspectPartition(mem)
	}
}

// intendLeave is invoked when we get a message indicating
// an intention to leave. Returns true if we should re-broadcast
func (s *Serf) intendLeave(l *leave) bool {
	s.memberLock.Lock()
	defer s.memberLock.Unlock()

	// Check if we know about this node
	mem, ok := s.members[l.Node]
	if !ok {
		return false // unknown, don't rebroadcast
	}

	// If the node is currently alive, then mark as a pending leave
	// and re-broadcast
	if mem.Status == StatusAlive {
		mem.Status = StatusLeaving

		// Schedule a timer to unmark the intention after a timeout
		time.AfterFunc(s.conf.LeaveTimeout, func() { s.resetIntention(mem) })
		return true
	}

	// State update not relevant, ignore it
	return false
}

// resetIntention is called after the leaveTimeout period to
// transition a node from StatusLeaving back to StatusAlive if it
// has not yet left the cluster
func (s *Serf) resetIntention(mem *Member) {
	s.memberLock.Lock()
	defer s.memberLock.Unlock()

	if mem.Status == StatusLeaving {
		mem.Status = StatusAlive
	}
}

// forceRemove is invoked when we get a message indicating
// a downed node should be force removed. Returns true if we should re-broadcast
func (s *Serf) forceRemove(r *remove) bool {
	s.memberLock.Lock()
	defer s.memberLock.Unlock()

	// Lookup the node, if unknown don't rebroadcast
	mem, ok := s.members[r.Node]
	if !ok {
		return false
	}

	// If the node is alive, or has left, do nothing
	if mem.Status == StatusAlive || mem.Status == StatusLeaving || mem.Status == StatusLeft {
		return false
	}

	// Update the status to Left
	mem.Status = StatusLeft

	// Remove from failed list
	s.failedMembers = removeOldMember(s.failedMembers, mem)

	// Add to the left list
	s.leftMembers = append(s.leftMembers, &oldMember{member: mem, time: time.Now()})
	// Unsuspect a partition
	s.unsuspectPartition(mem)

	// Propogate the status update
	return true
}
