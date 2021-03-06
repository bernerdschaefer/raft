package raft_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/peterbourgon/raft"
	"io"
	"log"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func init() {
	rand.Seed(time.Now().UnixNano())
	log.SetFlags(log.Lmicroseconds)
}

func TestFollowerToCandidate(t *testing.T) {
	log.SetOutput(&bytes.Buffer{})
	defer log.SetOutput(os.Stdout)
	oldMin, oldMax := raft.ResetElectionTimeoutMs(25, 50)
	defer raft.ResetElectionTimeoutMs(oldMin, oldMax)

	noop := func([]byte) ([]byte, error) { return []byte{}, nil }
	server := raft.NewServer(1, &bytes.Buffer{}, noop)
	server.SetPeers(raft.MakePeers(nonresponsivePeer(2), nonresponsivePeer(3)))
	if server.State() != raft.Follower {
		t.Fatalf("didn't start as Follower")
	}

	server.Start()
	defer func() { server.Stop(); t.Logf("server stopped") }()

	time.Sleep(raft.MaximumElectionTimeout())

	cutoff := time.Now().Add(2 * raft.MinimumElectionTimeout())
	backoff := raft.BroadcastInterval()
	for {
		if time.Now().After(cutoff) {
			t.Fatal("failed to become Candidate")
		}
		if state := server.State(); state != raft.Candidate {
			time.Sleep(backoff)
			backoff *= 2
			continue
		}
		t.Logf("became Candidate")
		break
	}
}

func TestCandidateToLeader(t *testing.T) {
	log.SetOutput(&bytes.Buffer{})
	defer log.SetOutput(os.Stdout)
	oldMin, oldMax := raft.ResetElectionTimeoutMs(25, 50)
	defer raft.ResetElectionTimeoutMs(oldMin, oldMax)

	noop := func([]byte) ([]byte, error) { return []byte{}, nil }
	server := raft.NewServer(1, &bytes.Buffer{}, noop)
	server.SetPeers(raft.MakePeers(nonresponsivePeer(1), approvingPeer(2), nonresponsivePeer(3)))
	server.Start()
	defer func() { server.Stop(); t.Logf("server stopped") }()

	time.Sleep(raft.MaximumElectionTimeout())

	cutoff := time.Now().Add(2 * raft.MaximumElectionTimeout())
	backoff := raft.BroadcastInterval()
	for {
		if time.Now().After(cutoff) {
			t.Fatal("failed to become Leader")
		}
		if state := server.State(); state != raft.Leader {
			time.Sleep(backoff)
			backoff *= 2
			continue
		}
		t.Logf("became Leader")
		break
	}
}

func TestFailedElection(t *testing.T) {
	log.SetOutput(&bytes.Buffer{})
	defer log.SetOutput(os.Stdout)
	oldMin, oldMax := raft.ResetElectionTimeoutMs(25, 50)
	defer raft.ResetElectionTimeoutMs(oldMin, oldMax)

	noop := func([]byte) ([]byte, error) { return []byte{}, nil }
	server := raft.NewServer(1, &bytes.Buffer{}, noop)
	server.SetPeers(raft.MakePeers(disapprovingPeer(2), nonresponsivePeer(3)))
	server.Start()
	defer func() { server.Stop(); t.Logf("server stopped") }()

	time.Sleep(2 * raft.ElectionTimeout())
	if server.State() == raft.Leader {
		t.Fatalf("erroneously became Leader")
	}
	t.Logf("remained %s", server.State())
}

func TestSimpleConsensus(t *testing.T) {
	logBuffer := &bytes.Buffer{}
	log.SetOutput(logBuffer)
	defer log.SetOutput(os.Stdout)
	defer printOnFailure(t, logBuffer)
	oldMin, oldMax := raft.ResetElectionTimeoutMs(25, 50)
	defer raft.ResetElectionTimeoutMs(oldMin, oldMax)

	type SetValue struct {
		Value int32 `json:"value"`
	}

	var i1, i2, i3 int32

	applyValue := func(id uint64, i *int32) func([]byte) ([]byte, error) {
		return func(cmd []byte) ([]byte, error) {
			var sv SetValue
			if err := json.Unmarshal(cmd, &sv); err != nil {
				return []byte{}, err
			}
			atomic.StoreInt32(i, sv.Value)
			return json.Marshal(map[string]interface{}{"applied_to_server": id, "applied_value": sv.Value})
		}
	}

	s1 := raft.NewServer(1, &bytes.Buffer{}, applyValue(1, &i1))
	s2 := raft.NewServer(2, &bytes.Buffer{}, applyValue(2, &i2))
	s3 := raft.NewServer(3, &bytes.Buffer{}, applyValue(3, &i3))

	s1Responses := &synchronizedBuffer{}
	s2Responses := &synchronizedBuffer{}
	s3Responses := &synchronizedBuffer{}
	defer func(sb *synchronizedBuffer) { t.Logf("s1 responses: %s", sb.String()) }(s1Responses)
	defer func(sb *synchronizedBuffer) { t.Logf("s2 responses: %s", sb.String()) }(s2Responses)
	defer func(sb *synchronizedBuffer) { t.Logf("s3 responses: %s", sb.String()) }(s3Responses)

	peers := raft.MakePeers(
		raft.NewLocalPeer(s1),
		raft.NewLocalPeer(s2),
		raft.NewLocalPeer(s3),
	)

	s1.SetPeers(peers)
	s2.SetPeers(peers)
	s3.SetPeers(peers)

	s1.Start()
	s2.Start()
	s3.Start()
	defer s1.Stop()
	defer s2.Stop()
	defer s3.Stop()

	var v int32 = 42
	cmd, _ := json.Marshal(SetValue{v})

	response := make(chan []byte, 1)
	func() {
		for {
			switch err := s1.Command(cmd, response); err {
			case nil:
				return
			case raft.ErrUnknownLeader:
				time.Sleep(raft.MinimumElectionTimeout())
			default:
				t.Fatal(err)
			}
		}
	}()

	r, ok := <-response
	if ok {
		s1Responses.Write(r)
	} else {
		t.Logf("didn't receive command response")
	}

	ticker := time.Tick(raft.BroadcastInterval())
	timeout := time.After(1 * time.Second)
	for {
		select {
		case <-ticker:
			i1l := atomic.LoadInt32(&i1)
			i2l := atomic.LoadInt32(&i2)
			i3l := atomic.LoadInt32(&i3)
			t.Logf("i1=%02d i2=%02d i3=%02d", i1l, i2l, i3l)
			if i1l == v && i2l == v && i3l == v {
				t.Logf("success!")
				return
			}

		case <-timeout:
			t.Fatal("timeout")
		}
	}
}

func TestOrdering_1Server(t *testing.T) {
	testOrderTimeout(t, 1, 5*time.Second)
}

func TestOrdering_2Servers(t *testing.T) {
	testOrderTimeout(t, 2, 5*time.Second)
}

func TestOrdering_3Servers(t *testing.T) {
	testOrderTimeout(t, 3, 5*time.Second)
}

func TestOrdering_4Servers(t *testing.T) {
	testOrderTimeout(t, 4, 5*time.Second)
}

func TestOrdering_5Servers(t *testing.T) {
	testOrderTimeout(t, 5, 5*time.Second)
}

func TestOrdering_6Servers(t *testing.T) {
	testOrderTimeout(t, 6, 5*time.Second)
}

func testOrderTimeout(t *testing.T, nServers int, timeout time.Duration) {
	logBuffer := &bytes.Buffer{}
	log.SetOutput(logBuffer)
	defer log.SetOutput(os.Stdout)
	defer printOnFailure(t, logBuffer)
	oldMin, oldMax := raft.ResetElectionTimeoutMs(50, 100)
	defer raft.ResetElectionTimeoutMs(oldMin, oldMax)

	done := make(chan struct{})
	go func() { testOrder(t, nServers); close(done) }()
	select {
	case <-done:
		break
	case <-time.After(timeout):
		t.Fatalf("timeout (infinite loop?)")
	}
}

func testOrder(t *testing.T, nServers int) {
	values := rand.Perm(8 + rand.Intn(16))

	// command and response
	type send struct {
		Send int `json:"send"`
	}
	type recv struct {
		Recv int `json:"recv"`
	}
	do := func(sb *synchronizedBuffer) func(buf []byte) ([]byte, error) {
		return func(buf []byte) ([]byte, error) {
			sb.Write(buf)                           // write incoming message
			var s send                              // decode incoming message
			json.Unmarshal(buf, &s)                 // ...
			return json.Marshal(recv{Recv: s.Send}) // write outgoing message
		}
	}

	// set up the cluster
	servers := []*raft.Server{}        // server components
	storage := []*bytes.Buffer{}       // persistent log storage
	buffers := []*synchronizedBuffer{} // the "state machine" for each server
	for i := 0; i < nServers; i++ {
		buffers = append(buffers, &synchronizedBuffer{})
		storage = append(storage, &bytes.Buffer{})
		servers = append(servers, raft.NewServer(uint64(i+1), storage[i], do(buffers[i])))
	}
	peers := raft.Peers{}
	for _, server := range servers {
		peers[server.Id()] = raft.NewLocalPeer(server)
	}
	for _, server := range servers {
		server.SetPeers(peers)
	}

	// define cmds
	cmds := []send{}
	for _, v := range values {
		cmds = append(cmds, send{v})
	}

	// the expected "state-machine" output of applying each command
	expectedBuffer := &synchronizedBuffer{}
	for _, cmd := range cmds {
		buf, _ := json.Marshal(cmd)
		expectedBuffer.Write(buf)
	}

	// boot up the cluster
	for _, server := range servers {
		server.Start()
		defer func(server0 *raft.Server) {
			log.Printf("issuing stop command to server %d", server0.Id())
			server0.Stop()
		}(server)
	}

	// send commands
	for i, cmd := range cmds {
		id := uint64(rand.Intn(nServers)) + 1
		peer := peers[id]
		buf, _ := json.Marshal(cmd)
		response := make(chan []byte, 1)
	retry:
		for {
			log.Printf("command=%d/%d peer=%d: sending %s", i+1, len(cmds), id, buf)
			switch err := peer.Command(buf, response); err {
			case nil:
				log.Printf("command=%d/%d peer=%d: OK", i+1, len(cmds), id)
				break retry
			case raft.ErrUnknownLeader, raft.ErrDeposed:
				log.Printf("command=%d/%d peer=%d: failed (%s) -- will retry", i+1, len(cmds), id, err)
				time.Sleep(raft.ElectionTimeout())
				continue
			case raft.ErrTimeout:
				log.Printf("command=%d/%d peer=%d: timed out -- assume it went through", i+1, len(cmds), id)
				break retry
			default:
				t.Fatalf("command=%d/%d peer=%d: failed (%s) -- fatal", i+1, len(cmds), id, err)
			}
		}
		r, ok := <-response
		if !ok {
			log.Printf("command=%d/%d peer=%d: truncated, will retry", i+1, len(cmds), id)
			response = make(chan []byte, 1) // channel was closed, must re-make
			goto retry
		}
		log.Printf("command=%d/%d peer=%d: OK, got response %s", i+1, len(cmds), id, string(r))
	}

	// done sending
	log.Printf("testOrder done sending %d command(s) to network", len(cmds))

	// check the buffers (state machines)
	for i, sb := range buffers {
		for {
			expected, got := expectedBuffer.String(), sb.String()
			if len(got) < len(expected) {
				t.Logf("server %d: not yet fully replicated, will check again", i+1)
				time.Sleep(raft.BroadcastInterval())
				continue // retry
			}
			if expected != got {
				t.Errorf("server %d: fully replicated, expected\n\t%s, got\n\t%s", i+1, expected, got)
				break
			}
			t.Logf("server %d: %s OK", i+1, got)
			break
		}
	}
}

//
//
//

func printOnFailure(t *testing.T, r io.Reader) {
	if !t.Failed() {
		return
	}
	rd := bufio.NewReader(r)
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		t.Logf("> %s", line)
	}
}

type synchronizedBuffer struct {
	sync.RWMutex
	buf bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.buf.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.RLock()
	defer b.RUnlock()
	return b.buf.String()
}

type nonresponsivePeer uint64

func (p nonresponsivePeer) Id() uint64 { return uint64(p) }
func (p nonresponsivePeer) AppendEntries(raft.AppendEntries) raft.AppendEntriesResponse {
	return raft.AppendEntriesResponse{}
}
func (p nonresponsivePeer) RequestVote(raft.RequestVote) raft.RequestVoteResponse {
	return raft.RequestVoteResponse{}
}
func (p nonresponsivePeer) Command([]byte, chan []byte) error {
	return fmt.Errorf("not implemented")
}

type approvingPeer uint64

func (p approvingPeer) Id() uint64 { return uint64(p) }
func (p approvingPeer) AppendEntries(raft.AppendEntries) raft.AppendEntriesResponse {
	return raft.AppendEntriesResponse{}
}
func (p approvingPeer) RequestVote(rv raft.RequestVote) raft.RequestVoteResponse {
	return raft.RequestVoteResponse{
		Term:        rv.Term,
		VoteGranted: true,
	}
}
func (p approvingPeer) Command([]byte, chan []byte) error {
	return fmt.Errorf("not implemented")
}

type disapprovingPeer uint64

func (p disapprovingPeer) Id() uint64 { return uint64(p) }
func (p disapprovingPeer) AppendEntries(raft.AppendEntries) raft.AppendEntriesResponse {
	return raft.AppendEntriesResponse{}
}
func (p disapprovingPeer) RequestVote(rv raft.RequestVote) raft.RequestVoteResponse {
	return raft.RequestVoteResponse{
		Term:        rv.Term,
		VoteGranted: false,
	}
}
func (p disapprovingPeer) Command([]byte, chan []byte) error {
	return fmt.Errorf("not implemented")
}
