package rafthttp_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/peterbourgon/raft"
	"github.com/peterbourgon/raft/http"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestId(t *testing.T) {
	id := uint64(33)
	s := rafthttp.NewServer(&echoServer{
		id:  id,
		aer: raft.AppendEntriesResponse{},
		rvr: raft.RequestVoteResponse{}},
	)
	m := newMockMux()
	s.Install(m)

	req, _ := http.NewRequest("GET", "", &bytes.Buffer{})
	resp, err := m.Call(rafthttp.IdPath, req)
	if err != nil {
		t.Fatal(err)
	}

	gotId, err := strconv.ParseUint(string(resp), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if gotId != id {
		t.Fatalf("expected %d, got %d", id, gotId)
	}
}

func TestCommand(t *testing.T) {
	s := rafthttp.NewServer(&echoServer{
		id:  1,
		aer: raft.AppendEntriesResponse{},
		rvr: raft.RequestVoteResponse{}},
	)
	m := newMockMux()
	s.Install(m)

	cmd := `{"foo":123}`
	req, _ := http.NewRequest("POST", "", bytes.NewBufferString(cmd))
	resp, err := m.Call(rafthttp.CommandPath, req)
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Compare([]byte(cmd), resp) != 0 {
		t.Fatalf("expected '%s', got '%s'", cmd, string(resp))
	}
}

func TestAppendEntries(t *testing.T) {
	aer := raft.AppendEntriesResponse{
		Term:    3,
		Success: true,
	}
	s := rafthttp.NewServer(&echoServer{
		id:  1,
		aer: aer,
		rvr: raft.RequestVoteResponse{},
	})
	m := newMockMux()
	s.Install(m)

	var body bytes.Buffer
	json.NewEncoder(&body).Encode(raft.AppendEntries{})
	req, _ := http.NewRequest("POST", "", &body)
	resp, err := m.Call(rafthttp.AppendEntriesPath, req)
	if err != nil {
		t.Fatal(err)
	}

	var expected bytes.Buffer
	json.NewEncoder(&expected).Encode(aer)
	if bytes.Compare(resp, expected.Bytes()) != 0 {
		t.Fatalf("expected '%s', got '%s'", expected.String(), string(resp))
	}
}

func TestRequestVote(t *testing.T) {
	rvr := raft.RequestVoteResponse{
		Term:        5,
		VoteGranted: true,
	}
	s := rafthttp.NewServer(&echoServer{
		id:  1,
		aer: raft.AppendEntriesResponse{},
		rvr: rvr,
	})
	m := newMockMux()
	s.Install(m)

	var body bytes.Buffer
	json.NewEncoder(&body).Encode(raft.RequestVote{})
	req, _ := http.NewRequest("POST", "", &body)
	resp, err := m.Call(rafthttp.RequestVotePath, req)
	if err != nil {
		t.Fatal(err)
	}

	var expected bytes.Buffer
	json.NewEncoder(&expected).Encode(rvr)
	if bytes.Compare(resp, expected.Bytes()) != 0 {
		t.Fatalf("expected '%s', got '%s'", expected.String(), string(resp))
	}
}

type mockMux struct {
	registry map[string]http.HandlerFunc
}

func newMockMux() *mockMux {
	return &mockMux{registry: map[string]http.HandlerFunc{}}
}

func (m *mockMux) HandleFunc(path string, h func(http.ResponseWriter, *http.Request)) {
	m.registry[path] = h
}

func (m *mockMux) Call(path string, r *http.Request) ([]byte, error) {
	handler, ok := m.registry[path]
	if !ok {
		return []byte{}, fmt.Errorf("invalid path")
	}

	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != http.StatusOK {
		return []byte{}, fmt.Errorf("HTTP %d", w.Code)
	}

	return w.Body.Bytes(), nil
}

type echoServer struct {
	id  uint64
	aer raft.AppendEntriesResponse
	rvr raft.RequestVoteResponse
}

func (p *echoServer) Id() uint64 { return p.id }
func (p *echoServer) AppendEntries(raft.AppendEntries) raft.AppendEntriesResponse {
	return p.aer
}
func (p *echoServer) RequestVote(rv raft.RequestVote) raft.RequestVoteResponse {
	return p.rvr
}
func (p *echoServer) Command(cmd []byte, response chan []byte) error {
	go func() { response <- cmd }()
	return nil
}
