package raft

import (
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"sync"
	"time"
)

const (
	Follower  = "Follower"
	Candidate = "Candidate"
	Leader    = "Leader"
)

var (
	MinimumElectionTimeoutMs = 250
)

var (
	ErrNotLeader             = errors.New("not the leader")
	ErrDeposed               = errors.New("deposed during replication")
	ErrAppendEntriesRejected = errors.New("AppendEntries RPC rejected")
)

// ElectionTimeout returns a variable time.Duration, between
// MinimumElectionTimeoutMs and twice that value.
func ElectionTimeout() time.Duration {
	n := rand.Intn(MinimumElectionTimeoutMs)
	d := MinimumElectionTimeoutMs + n
	return time.Duration(d) * time.Millisecond
}

// BroadcastInterval returns the interval between heartbeats (AppendEntry RPCs)
// broadcast from the leader. It is MinimumElectionTimeoutMs / 10, as dictated
// by the spec: BroadcastInterval << ElectionTimeout << MTBF.
func BroadcastInterval() time.Duration {
	d := MinimumElectionTimeoutMs / 10
	return time.Duration(d) * time.Millisecond
}

// serverState is just a string protected by a mutex.
type serverState struct {
	sync.RWMutex
	value string
}

func (s *serverState) Get() string {
	s.RLock()
	defer s.RUnlock()
	return s.value
}

func (s *serverState) Set(value string) {
	s.Lock()
	defer s.Unlock()
	s.value = value
}

// serverTerm is just a uint64 protected by a mutex.
type serverTerm struct {
	sync.RWMutex
	value uint64
}

func (s *serverTerm) Get() uint64 {
	s.RLock()
	defer s.RUnlock()
	return s.value
}

func (s *serverTerm) Increment() {
	s.Lock()
	defer s.Unlock()
	s.value++
}

// Server is the agent that performs all of the Raft protocol logic.
// In a typical application, each running process that wants to be part of
// the distributed state machine will contain a Server component.
type Server struct {
	Id                uint64 // of this server, for elections and redirects
	state             *serverState
	term              uint64 // "current term number, which increases monotonically"
	vote              uint64 // who we voted for this term, if applicable
	log               *Log
	peers             Peers
	appendEntriesChan chan appendEntriesTuple
	requestVoteChan   chan requestVoteTuple
	commandChan       chan commandTuple
	electionTick      <-chan time.Time
}

// NewServer returns an initialized, un-started Server.
// The ID must be unique in the Raft network, and greater than 0.
// The store will be used by the distributed log as a persistence layer.
// The apply function will be called whenever a (user-domain) command has been
// safely replicated to this Server, and should be applied.
func NewServer(id uint64, store io.Writer, apply func([]byte) ([]byte, error)) *Server {
	if id <= 0 {
		panic("server id must be > 0")
	}

	s := &Server{
		Id:                id,
		state:             &serverState{value: Follower}, // "when servers start up they begin as followers"
		term:              1,                             // TODO is this correct?
		log:               NewLog(store, apply),
		peers:             nil,
		appendEntriesChan: make(chan appendEntriesTuple),
		requestVoteChan:   make(chan requestVoteTuple),
		commandChan:       make(chan commandTuple),
		electionTick:      time.NewTimer(ElectionTimeout()).C, // one-shot
	}
	return s
}

// SetPeers injects the set of Peers that this server will attempt to
// communicate with, in its Raft network. The set Peers should include a Peer
// that represents this server, so that Quorum is calculated correctly.
func (s *Server) SetPeers(p Peers) {
	s.peers = p
}

// State returns the current state: Follower, Candidate, or Leader.
func (s *Server) State() string {
	return s.state.Get()
}

// Start triggers the Server to begin communicating with its peers.
func (s *Server) Start() {
	go s.loop()
}

type commandTuple struct {
	Command  []byte
	Response chan []byte
	Err      chan error
}

// Command pushes a state-machine command through the Raft network.
// Once Raft has decided it's been safely replicated, the command is applied
// (via the apply function, passed at Server instantiation) and this function
// returns.
//
// Note that per Raft semantics, this method may block for some time, and can
// appear to fail (via a timeout) if we don't reach a quorum. But once the
// command is registered with the leader, Raft will try to replicate it to all
// servers, and won't give up until it succeeds. So, while Raft does guarantee
// command order from the perspective of the leader, the safest bet is to
// structure your commands so that they're idempotent.
func (s *Server) Command(cmd []byte) ([]byte, error) {
	t := commandTuple{cmd, make(chan []byte), make(chan error)}
	s.commandChan <- t
	select {
	case resp := <-t.Response:
		return resp, nil
	case err := <-t.Err:
		return []byte{}, err
	}
}

// AppendEntries processes the given RPC and returns the response.
// This is a public method only to facilitate the construction of Peers
// on arbitrary transports.
func (s *Server) AppendEntries(ae AppendEntries) AppendEntriesResponse {
	t := appendEntriesTuple{
		Request:  ae,
		Response: make(chan AppendEntriesResponse),
	}
	s.appendEntriesChan <- t
	return <-t.Response
}

// RequestVote processes the given RPC and returns the response.
// This is a public method only to facilitate the construction of Peers
// on arbitrary transports.
func (s *Server) RequestVote(rv RequestVote) RequestVoteResponse {
	t := requestVoteTuple{
		Request:  rv,
		Response: make(chan RequestVoteResponse),
	}
	s.requestVoteChan <- t
	return <-t.Response
}

//                                  times out,
//                                 new election
//     |                             .-----.
//     |                             |     |
//     v         times out,          |     v     receives votes from
// +----------+  starts election  +-----------+  majority of servers  +--------+
// | Follower |------------------>| Candidate |---------------------->| Leader |
// +----------+                   +-----------+                       +--------+
//     ^ ^                              |                                 |
//     | |    discovers current leader  |                                 |
//     | |                 or new term  |                                 |
//     | '------------------------------'                                 |
//     |                                                                  |
//     |                               discovers server with higher term  |
//     '------------------------------------------------------------------'
//
//

func (s *Server) loop() {
	for {
		switch state := s.State(); state {
		case Follower:
			s.followerSelect()
		case Candidate:
			s.candidateSelect()
		case Leader:
			s.leaderSelect()
		default:
			panic(fmt.Sprintf("unknown Server State '%s'", state))
		}
	}
}

func (s *Server) resetElectionTimeout() {
	s.electionTick = time.NewTimer(ElectionTimeout()).C
}

func (s *Server) logGeneric(format string, args ...interface{}) {
	prefix := fmt.Sprintf("id=%d term=%d state=%s: ", s.Id, s.term, s.State())
	log.Printf(prefix+format, args...)
}

func (s *Server) logAppendEntriesResponse(req AppendEntries, resp AppendEntriesResponse, stepDown bool) {
	s.logGeneric(
		"got AppendEntries, sz=%d prevIndex/Term=%d/%d commitIndex=%d: responded with success=%v (%s) stepDown=%v",
		len(req.Entries),
		req.PrevLogIndex,
		req.PrevLogTerm,
		req.CommitIndex,
		resp.Success,
		resp.reason,
		stepDown,
	)
}
func (s *Server) logRequestVoteResponse(req RequestVote, resp RequestVoteResponse, stepDown bool) {
	s.logGeneric(
		"got RequestVote, candidate=%d: responded with granted=%v (%s) stepDown=%v",
		req.CandidateId,
		resp.VoteGranted,
		resp.reason,
		stepDown,
	)
}

func (s *Server) followerSelect() {
	for {
		select {
		case commandTuple := <-s.commandChan:
			commandTuple.Err <- ErrNotLeader // TODO forward instead
			continue

		case <-s.electionTick:
			// 5.2 Leader election: "A follower increments its current term and
			// transitions to candidate state."
			s.logGeneric("election timeout, becoming candidate")
			s.term++
			s.state.Set(Candidate)
			s.resetElectionTimeout()
			return

		case t := <-s.appendEntriesChan:
			resp, stepDown := s.handleAppendEntries(t.Request)
			s.logAppendEntriesResponse(t.Request, resp, stepDown)
			t.Response <- resp

		case t := <-s.requestVoteChan:
			resp, stepDown := s.handleRequestVote(t.Request)
			s.logRequestVoteResponse(t.Request, resp, stepDown)
			t.Response <- resp
		}
	}
}

func (s *Server) candidateSelect() {
	// "[A server entering the candidate stage] issues RequestVote RPCs in
	// parallel to each of the other servers in the cluster. If the candidate
	// receives no response for an RPC, it reissues the RPC repeatedly until a
	// response arrives or the election concludes."

	responses, canceler := s.peers.Except(s.Id).RequestVotes(RequestVote{
		Term:         s.term,
		CandidateId:  s.Id,
		LastLogIndex: s.log.LastIndex(),
		LastLogTerm:  s.log.LastTerm(),
	})
	defer canceler.Cancel()
	votesReceived := 1 // already have a vote from myself
	votesRequired := s.peers.Quorum()
	s.logGeneric("election started, %d vote(s) required", votesRequired)

	// catch a bad state
	if votesReceived >= votesRequired {
		s.logGeneric("%d-node cluster; I win", s.peers.Count())
		s.state.Set(Leader)
		return
	}

	// "A candidate continues in this state until one of three things happens:
	// (a) it wins the election, (b) another server establishes itself as
	// leader, or (c) a period of time goes by with no winner."
	for {
		select {
		case commandTuple := <-s.commandChan:
			commandTuple.Err <- ErrNotLeader // TODO forward instead
			continue

		case r := <-responses:
			s.logGeneric("got vote: term=%d granted=%v", r.Term, r.VoteGranted)
			// "A candidate wins the election if it receives votes from a
			// majority of servers in the full cluster for the same term."
			if r.Term != s.term {
				// TODO what if r.Term > s.term? do we lose the election?
				continue
			}
			if r.VoteGranted {
				votesReceived++
			}
			// "Once a candidate wins an election, it becomes leader."
			if votesReceived >= votesRequired {
				s.logGeneric("%d >= %d: win", votesReceived, votesRequired)
				s.state.Set(Leader)
				return // win
			}

		case t := <-s.appendEntriesChan:
			// "While waiting for votes, a candidate may receive an
			// AppendEntries RPC from another server claiming to be leader.
			// If the leader's term (included in its RPC) is at least as
			// large as the candidate's current term, then the candidate
			// recognizes the leader as legitimate and steps down, meaning
			// that it returns to follower state."
			resp, stepDown := s.handleAppendEntries(t.Request)
			s.logAppendEntriesResponse(t.Request, resp, stepDown)
			t.Response <- resp
			if stepDown {
				s.logGeneric("stepping down to Follower")
				s.state.Set(Follower)
				return // lose
			}

		case t := <-s.requestVoteChan:
			// We can also be defeated by a more recent candidate
			resp, stepDown := s.handleRequestVote(t.Request)
			s.logRequestVoteResponse(t.Request, resp, stepDown)
			t.Response <- resp
			if stepDown {
				s.logGeneric("stepping down to Follower")
				s.state.Set(Follower)
				return // lose
			}

		case <-s.electionTick: //  "a period of time goes by with no winner"
			s.logGeneric("election ended with no winner")
			s.resetElectionTimeout()
			return // draw
		}
	}
}

//
//
//

type nextIndex struct {
	sync.RWMutex
	m map[uint64]uint64 // followerId: nextIndex
}

func newNextIndex(peers Peers, defaultNextIndex uint64) *nextIndex {
	ni := &nextIndex{
		m: map[uint64]uint64{},
	}
	for id, _ := range peers {
		ni.m[id] = defaultNextIndex
	}
	return ni
}

func (ni *nextIndex) PrevLogIndex(id uint64) uint64 {
	ni.RLock()
	defer ni.RUnlock()
	if _, ok := ni.m[id]; !ok {
		panic(fmt.Sprintf("peer %d not found", id))
	}
	return ni.m[id]
}

func (ni *nextIndex) Decrement(id uint64) {
	ni.Lock()
	defer ni.Unlock()
	if i, ok := ni.m[id]; !ok {
		panic(fmt.Sprintf("peer %d not found", id))
	} else if i > 0 {
		// This value can reach 0, so it should not be passed
		// directly to log.EntriesAfter.
		ni.m[id]--
	}
}

func (ni *nextIndex) Set(id, index uint64) {
	ni.Lock()
	defer ni.Unlock()
	ni.m[id] = index
}

// Flush generates and forwards an AppendEntries request that attempts to bring
// the given follower "in sync" with our log. It's idempotent, so it's used for
// both heartbeats and replicating commands.
//
// The AppendEntries request we build represents our best attempt at a "delta"
// between our log and the follower's log. The passed nextIndex structure
// manages that state.
func (s *Server) Flush(peer Peer, ni *nextIndex) error {
	peerId := peer.Id()
	currentTerm := s.term
	prevLogIndex := ni.PrevLogIndex(peerId)
	entries, prevLogTerm := s.log.EntriesAfter(prevLogIndex, currentTerm)
	commitIndex := s.log.CommitIndex()
	resp := peer.AppendEntries(AppendEntries{
		Term:         currentTerm,
		LeaderId:     s.Id,
		PrevLogIndex: prevLogIndex,
		PrevLogTerm:  prevLogTerm,
		Entries:      entries,
		CommitIndex:  commitIndex,
	})
	if resp.Term > currentTerm {
		return ErrDeposed
	}
	if !resp.Success {
		ni.Decrement(peerId)
		return ErrAppendEntriesRejected
	}

	if len(entries) > 0 {
		ni.Set(peer.Id(), entries[len(entries)-1].Index)
	}
	return nil
}

func (s *Server) leaderSelect() {
	// 5.3 Log replication: "The leader maintains a nextIndex for each follower,
	// which is the index of the next log entry the leader will send to that
	// follower. When a leader first comes to power it initializes all nextIndex
	// values to the index just after the last one in its log."
	ni := newNextIndex(s.peers, s.log.LastIndex()+1)

	heartbeatTick := time.Tick(BroadcastInterval())
	for {
		select {
		case commandTuple := <-s.commandChan:
			// Append the command to our (leader) log
			currentTerm := s.term
			entry := LogEntry{
				Index:   s.log.LastIndex() + 1,
				Term:    currentTerm,
				Command: commandTuple.Command,
			}
			if err := s.log.AppendEntry(entry); err != nil {
				commandTuple.Err <- err
				continue
			}

			// From here forward, we'll always attempt to replicate the command
			// to our followers, via the heartbeat mechanism. This timeout is
			// purely for our present response to the client.
			timeout := time.After(ElectionTimeout())

			// Scatter flush requests to all peers
			responses := make(chan error, len(s.peers))
			for _, peer := range s.peers.Except(s.Id) {
				go func(peer0 Peer) {
					err := s.Flush(peer0, ni)
					if err != nil {
						s.logGeneric("replicate: flush to %d: %s", peer0.Id(), err)
					}
					responses <- err
				}(peer)
			}

			// Gather responses and signal a deposition or successful commit
			committed := make(chan struct{})
			deposed := make(chan struct{})
			go func() {
				have, required := 1, s.peers.Quorum()
				for err := range responses {
					if err == ErrDeposed {
						close(deposed)
						return
					}
					if err == nil {
						have++
					}
					if have > required {
						close(committed)
						return
					}
				}
			}()

			// Return a response
			select {
			case <-deposed:
				commandTuple.Err <- ErrDeposed
				return
			case <-timeout:
				commandTuple.Err <- ErrTimeout
				continue
			case <-committed:
				// Commit our local log
				if err := s.log.CommitTo(entry.Index); err != nil {
					panic(err)
				}
				// Push out another update, to sync that commit
				for _, peer := range s.peers.Except(s.Id) {
					s.Flush(peer, ni) // TODO I think this is OK?
				}
				commandTuple.Response <- []byte{} // TODO actual response
				continue
			}

		case <-heartbeatTick:
			// Heartbeats attempt to sync the follower log with ours.
			// That requires per-follower state in the form of nextIndex.
			recipients := s.peers.Except(s.Id)
			wg := sync.WaitGroup{}
			wg.Add(len(recipients))
			for _, peer := range recipients {
				go func(peer0 Peer) {
					defer wg.Done()
					err := s.Flush(peer0, ni)
					if err != nil {
						s.logGeneric(
							"heartbeat: flush to %d: %s (nextIndex now %d)",
							peer0.Id(),
							err,
							ni.PrevLogIndex(peer0.Id()),
						)
					}
				}(peer)
			}
			wg.Wait()

		case t := <-s.appendEntriesChan:
			resp, stepDown := s.handleAppendEntries(t.Request)
			s.logAppendEntriesResponse(t.Request, resp, stepDown)
			t.Response <- resp
			if stepDown {
				s.state.Set(Follower)
				return
			}

		case t := <-s.requestVoteChan:
			resp, stepDown := s.handleRequestVote(t.Request)
			s.logRequestVoteResponse(t.Request, resp, stepDown)
			t.Response <- resp
			if stepDown {
				s.state.Set(Follower)
				return
			}
		}
	}
}

func (s *Server) handleRequestVote(r RequestVote) (RequestVoteResponse, bool) {
	// Spec is ambiguous here; basing this (loosely!) on benbjohnson's impl

	// If the request is from an old term, reject
	if r.Term < s.term {
		return RequestVoteResponse{
			Term:        s.term,
			VoteGranted: false,
			reason:      fmt.Sprintf("Term %d < %d", r.Term, s.term),
		}, false
	}

	// If the request is from a newer term, reset our state
	stepDown := false
	if r.Term > s.term {
		s.term = r.Term
		s.vote = 0
		stepDown = true
	}

	// If we've already voted for someone else this term, reject
	if s.vote != 0 && s.vote != r.CandidateId {
		return RequestVoteResponse{
			Term:        s.term,
			VoteGranted: false,
			reason:      fmt.Sprintf("already cast vote for %d", s.vote),
		}, stepDown
	}

	// If the candidate log isn't at least as recent as ours, reject
	if s.log.LastIndex() > r.LastLogIndex || s.log.LastTerm() > r.LastLogTerm {
		return RequestVoteResponse{
			Term:        s.term,
			VoteGranted: false,
			reason: fmt.Sprintf(
				"our index/term %d/%d > %d/%d",
				s.log.LastIndex(),
				s.log.LastTerm(),
				r.LastLogIndex,
				r.LastLogTerm,
			),
		}, stepDown
	}

	// We passed all the tests: cast vote in favor
	s.vote = r.CandidateId
	s.resetElectionTimeout() // TODO why?
	return RequestVoteResponse{
		Term:        s.term,
		VoteGranted: true,
	}, stepDown
}

func (s *Server) handleAppendEntries(r AppendEntries) (AppendEntriesResponse, bool) {
	// Spec is ambiguous here; basing this on benbjohnson's impl

	// Maybe a nicer way to handle this is to define explicit handler functions
	// for each Server state. Then, we won't try to hide too much logic (i.e.
	// too many protocol rules) in one code path.

	// If the request is from an old term, reject
	if r.Term < s.term {
		return AppendEntriesResponse{
			Term:    s.term,
			Success: false,
			reason:  fmt.Sprintf("Term %d < %d", r.Term, s.term),
		}, false
	}

	// If the request is from a newer term, reset our state
	stepDown := false
	if r.Term > s.term {
		s.term = r.Term
		s.vote = 0
		stepDown = true
	}

	// In any case, reset our election timeout
	s.resetElectionTimeout()

	// // Special case
	// if len(r.Entries) == 0 && r.CommitIndex == s.log.CommitIndex() {
	// 	return AppendEntriesResponse{
	// 		Term:    s.term,
	// 		Success: true,
	// 		reason:  "nothing to do",
	// 	}, stepDown
	// }

	// Reject if log doesn't contain a matching previous entry
	if err := s.log.EnsureLastIs(r.PrevLogIndex, r.PrevLogTerm); err != nil {
		return AppendEntriesResponse{
			Term:    s.term,
			Success: false,
			reason: fmt.Sprintf(
				"while ensuring last log entry had index=%d term=%d: error: %s",
				r.PrevLogIndex,
				r.PrevLogTerm,
				err,
			),
		}, stepDown
	}

	// Append entries to the log
	for i, entry := range r.Entries {
		if err := s.log.AppendEntry(entry); err != nil {
			return AppendEntriesResponse{
				Term:    s.term,
				Success: false,
				reason: fmt.Sprintf(
					"AppendEntry %d/%d failed: %s",
					i+1,
					len(r.Entries),
					err,
				),
			}, stepDown
		}
	}

	// Commit up to the commit index
	if r.CommitIndex > 0 { // TODO perform this check, or let it fail?
		if err := s.log.CommitTo(r.CommitIndex); err != nil {
			return AppendEntriesResponse{
				Term:    s.term,
				Success: false,
				reason:  fmt.Sprintf("CommitTo(%d) failed: %s", r.CommitIndex, err),
			}, stepDown
		}
	}

	// all good
	return AppendEntriesResponse{
		Term:    s.term,
		Success: true,
	}, stepDown
}