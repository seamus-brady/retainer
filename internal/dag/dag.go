// Package dag holds the in-memory cycle DAG: parent → child cycle
// relationships. Mirrors Springdrift's Librarian-owns-ETS pattern — one
// goroutine owns the map, all reads and writes go through its inbox.
//
// Volatile by design: process restart loses the DAG. The cycle log on disk
// has the same data and can rebuild the DAG if a tool ever needs that.
//
// Node types match Springdrift's set: CognitiveCycle is the cog cycle
// root; SchedulerCycle, AgentCycle, and DeputyCycle are reserved for
// when those subsystems land as children of (or alongside) the cog
// cycle. The curator is a service, not a cycle node — it parents its
// cycle-log events to the cog cycle but does NOT create DAG nodes,
// matching Springdrift's pattern.
//
// The DAG's purpose is the cycle tree: cog cycles spread out into
// agent/deputy/scheduler children via parent_id, accessible via
// Children(parent_id).
package dag

import (
	"context"
	"time"
)

type CycleID string

type NodeType string

const (
	NodeCognitive NodeType = "cognitive"
	NodeScheduler NodeType = "scheduler"
	NodeAgent     NodeType = "agent"
	NodeDeputy    NodeType = "deputy"
)

type Status string

const (
	StatusInProgress Status = "in_progress"
	StatusComplete   Status = "complete"
	StatusError      Status = "error"
	StatusAbandoned  Status = "abandoned"
)

type Node struct {
	ID           CycleID
	ParentID     CycleID
	Type         NodeType
	Status       Status
	ErrorMessage string // populated when Status != Complete; for forensic introspection
	StartedAt    time.Time
	CompletedAt  time.Time
}

// Snapshot returns a value copy. The actor never hands out raw *Node
// pointers; callers always work with snapshots.
func (n Node) Snapshot() Node { return n }

// DAG is the goroutine-owned cycle graph.
type DAG struct {
	inbox chan dagMsg
	nodes map[CycleID]*Node
}

const inboxBufferSize = 64

func New() *DAG {
	return &DAG{
		inbox: make(chan dagMsg, inboxBufferSize),
		nodes: make(map[CycleID]*Node),
	}
}

// Run is the actor loop. Block until ctx is cancelled. Run under
// actor.Permanent in a supervisor.
func (d *DAG) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-d.inbox:
			d.handle(msg)
		}
	}
}

func (d *DAG) handle(msg dagMsg) {
	switch m := msg.(type) {
	case startCycle:
		d.nodes[m.id] = &Node{
			ID:        m.id,
			ParentID:  m.parentID,
			Type:      m.nodeType,
			Status:    StatusInProgress,
			StartedAt: m.at,
		}
	case completeCycle:
		if n, ok := d.nodes[m.id]; ok {
			n.Status = m.status
			n.CompletedAt = m.at
			n.ErrorMessage = m.errMsg
		}
	case getNode:
		var snap *Node
		if n, ok := d.nodes[m.id]; ok {
			s := n.Snapshot()
			snap = &s
		}
		m.reply <- snap
	case getChildren:
		var out []Node
		for _, n := range d.nodes {
			if n.ParentID == m.parentID {
				out = append(out, n.Snapshot())
			}
		}
		m.reply <- out
	}
}

// StartCycle records a new cycle. Fire-and-forget — returns once the
// message is on the inbox.
func (d *DAG) StartCycle(id, parentID CycleID, nodeType NodeType) {
	d.inbox <- startCycle{id: id, parentID: parentID, nodeType: nodeType, at: time.Now()}
}

// CompleteCycle marks an existing cycle done. errMsg is the user-or-system-
// readable error string when status indicates failure (Error / Abandoned /
// Blocked); empty for clean completions. Silent no-op if the cycle was
// never started.
func (d *DAG) CompleteCycle(id CycleID, status Status, errMsg string) {
	d.inbox <- completeCycle{id: id, status: status, at: time.Now(), errMsg: errMsg}
}

// Get returns a snapshot of a node by ID, or nil if no such cycle.
// Synchronous — blocks on the actor inbox.
func (d *DAG) Get(id CycleID) *Node {
	reply := make(chan *Node, 1)
	d.inbox <- getNode{id: id, reply: reply}
	return <-reply
}

// Children returns snapshots of all nodes whose ParentID matches.
// Synchronous.
func (d *DAG) Children(parentID CycleID) []Node {
	reply := make(chan []Node, 1)
	d.inbox <- getChildren{parentID: parentID, reply: reply}
	return <-reply
}

// dagMsg is the actor inbox sum type.
type dagMsg interface{ isDagMsg() }

type startCycle struct {
	id       CycleID
	parentID CycleID
	nodeType NodeType
	at       time.Time
}

type completeCycle struct {
	id     CycleID
	status Status
	at     time.Time
	errMsg string
}

type getNode struct {
	id    CycleID
	reply chan<- *Node
}

type getChildren struct {
	parentID CycleID
	reply    chan<- []Node
}

func (startCycle) isDagMsg()    {}
func (completeCycle) isDagMsg() {}
func (getNode) isDagMsg()       {}
func (getChildren) isDagMsg()   {}
