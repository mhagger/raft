// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.etcd.io/raft/v3/raftpb"
)

type peer struct {
	id          uint64
	name        string
	raftdir     string
	snapdir     string
	node        *raftNode
	proposeC    chan string
	confChangeC chan raftpb.ConfChange
}

func newPeer(id uint64) *peer {
	peer := peer{
		id:          id,
		name:        fmt.Sprintf("http://127.0.0.1:%d", 10000+(id-1)),
		raftdir:     fmt.Sprintf("raftexample-%d", id),
		snapdir:     fmt.Sprintf("raftexample-%d-snap", id),
		proposeC:    make(chan string, 1),
		confChangeC: make(chan raftpb.ConfChange, 1),
	}

	return &peer
}

func (peer *peer) start(fsm FSM, peerNames []string, join bool) {
	os.RemoveAll(peer.raftdir)
	os.RemoveAll(peer.snapdir)

	snapshotLogger := zap.NewExample()
	snapshotStorage, err := newSnapshotStorage(snapshotLogger, peer.snapdir)
	if err != nil {
		log.Fatalf("raftexample: %v", err)
	}

	peer.node = startRaftNode(
		peer.id, peerNames, join,
		fsm, snapshotStorage,
		peer.proposeC, peer.confChangeC,
	)
}

// Cleanup cleans up temporary files used by the peer.
func (peer *peer) cleanup() {
	os.RemoveAll(peer.raftdir)
	os.RemoveAll(peer.snapdir)
}

type nullFSM struct {
	id uint64
}

func (fsm nullFSM) String() string {
	return fmt.Sprintf("node %d", fsm.id)
}

func (nullFSM) TakeSnapshot() ([]byte, error) {
	return nil, nil
}

func (nullFSM) RestoreSnapshot(_ []byte) error {
	return nil
}

func (nullFSM) ApplyCommits(commit *commit) error {
	close(commit.applyDoneC)
	return nil
}

type cluster struct {
	peerNames []string
	peers     []*peer
}

// newCluster creates a cluster of n nodes
func newCluster(fsms ...FSM) *cluster {
	clus := cluster{}

	for i := range fsms {
		peer := newPeer(uint64(i + 1))
		clus.peers = append(clus.peers, peer)
		clus.peerNames = append(clus.peerNames, peer.name)
	}

	for i, peer := range clus.peers {
		peer.start(fsms[i], clus.peerNames, false)
	}

	return &clus
}

// Cleanup cleans up the peers in the test cluster.
func (clus *cluster) Cleanup() {
	for _, peer := range clus.peers {
		peer.cleanup()
	}
}

// Close closes all cluster nodes and returns an error if any failed.
func (clus *cluster) Close() (err error) {
	for _, peer := range clus.peers {
		peer := peer
		go func() {
			//nolint:revive
			for range peer.node.commitC {
				// drain pending commits
			}
		}()
		close(peer.proposeC)
		// wait for channel to close
		<-peer.node.Done()
		if erri := peer.node.Err(); erri != nil {
			err = erri
		}
	}
	clus.Cleanup()
	return err
}

func (clus *cluster) closeNoErrors(t *testing.T) {
	t.Log("closing cluster...")
	if err := clus.Close(); err != nil {
		t.Fatal(err)
	}
	t.Log("closing cluster [done]")
}

type feedbackFSM struct {
	nullFSM
	peer     *peer
	reEcho   int
	expected int
	received int
}

func newFeedbackFSM(id uint64, reEcho int, expected int) *feedbackFSM {
	return &feedbackFSM{
		nullFSM:  nullFSM{id},
		reEcho:   reEcho,
		expected: expected,
	}
}

func (fsm *feedbackFSM) ApplyCommits(commit *commit) error {
	for _, msg := range commit.data {
		var originator, source, index int
		if n, err := fmt.Sscanf(msg, "foo%d-%d-%d", &originator, &source, &index); err != nil || n != 3 {
			panic(err)
		}
		if fsm.reEcho > 0 {
			fsm.peer.proposeC <- fmt.Sprintf("foo%d-%d-%d", originator, fsm.id, index+1)
			fsm.reEcho--
		}

		fsm.received++
		if fsm.received == fsm.expected {
			close(fsm.peer.proposeC)
		}
	}

	close(commit.applyDoneC)

	return nil
}

// TestProposeOnCommit starts three nodes and feeds commits back into the proposal
// channel. The intent is to ensure blocking on a proposal won't block raft progress.
func TestProposeOnCommit(t *testing.T) {
	// We generate one proposal for each node to kick things off, then
	// each node "echos" back the first 99 commits that it receives.
	// So the total number of commits that each node expects to see is
	// 300.
	fsms := []*feedbackFSM{
		newFeedbackFSM(1, 99, 300),
		newFeedbackFSM(2, 99, 300),
		newFeedbackFSM(3, 99, 300),
	}
	clus := newCluster(fsms[0], fsms[1], fsms[2])
	defer clus.Cleanup()

	for i := range fsms {
		fsms[i].peer = clus.peers[i]
	}

	var wg sync.WaitGroup
	for _, fsm := range fsms {
		fsm := fsm
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fsm.peer.node.ProcessCommits(); err != nil {
				t.Error("ProcessCommits returned error", err)
			}
		}()

		// Trigger the whole cascade by sending one message per node:
		wg.Add(1)
		go func() {
			defer wg.Done()
			fsm.peer.proposeC <- fmt.Sprintf("foo%d-%d-%d", fsm.id, fsm.id, 0)
		}()
	}

	wg.Wait()

	for _, fsm := range fsms {
		if fsm.received != fsm.expected {
			t.Errorf("node %d received %d commits (expected %d)", fsm.id, fsm.received, fsm.expected)
		}
	}
}

// TestCloseProposerBeforeReplay tests closing the producer before raft starts.
func TestCloseProposerBeforeReplay(t *testing.T) {
	clus := newCluster(nullFSM{1})
	// close before replay so raft never starts
	defer clus.closeNoErrors(t)
}

// TestCloseProposerInflight tests closing the producer while
// committed messages are being published to the client.
func TestCloseProposerInflight(t *testing.T) {
	clus := newCluster(nullFSM{1})
	defer clus.closeNoErrors(t)

	var wg sync.WaitGroup
	wg.Add(1)

	// some inflight ops
	go func() {
		defer wg.Done()
		clus.peers[0].proposeC <- "foo"
		clus.peers[0].proposeC <- "bar"
	}()

	// wait for one message
	if c, ok := <-clus.peers[0].node.commitC; !ok || c.data[0] != "foo" {
		t.Fatalf("Commit failed")
	}

	wg.Wait()
}

func TestPutAndGetKeyValue(t *testing.T) {
	peer := newPeer(1)
	defer func() {
		close(peer.proposeC)
		close(peer.confChangeC)
	}()

	kvs, fsm := newKVStore(peer.proposeC)
	peer.start(fsm, []string{peer.name}, false)
	defer peer.cleanup()

	go func() {
		if err := peer.node.ProcessCommits(); err != nil {
			log.Fatalf("raftexample: %v", err)
		}
	}()

	srv := httptest.NewServer(&httpKVAPI{
		store:       kvs,
		confChangeC: peer.confChangeC,
	})
	defer srv.Close()

	// wait server started
	time.Sleep(3 * time.Second)

	wantKey, wantValue := "test-key", "test-value"
	url := fmt.Sprintf("%s/%s", srv.URL, wantKey)
	body := bytes.NewBufferString(wantValue)
	cli := srv.Client()

	req, err := http.NewRequest(http.MethodPut, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/html; charset=utf-8")
	_, err = cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	// wait for a moment for processing message, otherwise get would be failed.
	time.Sleep(time.Second)

	resp, err := cli.Get(url)
	if err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if gotValue := string(data); wantValue != gotValue {
		t.Fatalf("expect %s, got %s", wantValue, gotValue)
	}
}

type commitWatcher struct {
	nullFSM
	C chan string
}

func newCommitWatcher(id uint64) *commitWatcher {
	return &commitWatcher{
		nullFSM: nullFSM{id},
		C:       make(chan string, 1),
	}
}

func (fsm *commitWatcher) ApplyCommits(commit *commit) error {
	log.Printf("applying commits to node %d!!!! %v", fsm.id, commit.data)
	for _, c := range commit.data {
		fsm.C <- c
	}
	close(commit.applyDoneC)
	return nil
}

// TestAddNewNode tests adding new node to the existing cluster.
func TestAddNewNode(t *testing.T) {
	var wg sync.WaitGroup
	defer wg.Wait()

	cw1 := newCommitWatcher(1)
	cw2 := newCommitWatcher(2)
	cw3 := newCommitWatcher(3)
	clus := newCluster(cw1, cw2, cw3)
	defer clus.closeNoErrors(t)

	for _, peer := range clus.peers {
		peer := peer
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := peer.node.ProcessCommits(); err != nil {
				t.Error("ProcessCommits returned error", err)
			}
		}()
	}

	peer4 := newPeer(4)
	defer func() {
		close(peer4.confChangeC)
		if peer4.proposeC != nil {
			close(peer4.proposeC)
		}
	}()

	cw4 := newCommitWatcher(4)
	peer4.start(cw4, append(clus.peerNames, peer4.name), true)
	defer peer4.cleanup()

	wg.Add(1)
	go func() {
		defer wg.Done()
		peer4.node.ProcessCommits()
	}()

	// Ask one of the old nodes to add the new one to the cluster:
	clus.peers[0].confChangeC <- raftpb.ConfChange{
		Type:    raftpb.ConfChangeAddNode,
		NodeID:  peer4.id,
		Context: []byte(peer4.name),
	}

	// Propose an update via the new node:
	peer4.proposeC <- "foo"

	// Verify that the update got committed to all of the nodes:
	for _, cw := range []*commitWatcher{cw1, cw2, cw3, cw4} {
		select {
		case c := <-cw.C:
			if c != "foo" {
				t.Errorf("Commit failed to node %d", cw.id)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("Timeout before commit arrived at node %d", cw.id)
		}
	}

	close(peer4.proposeC)
	peer4.proposeC = nil

	if err := peer4.node.Err(); err != nil {
		t.Error("ProcessCommits returned error", err)
	}
}

type snapshotWatcher struct {
	nullFSM
	C chan struct{}
}

func (sw snapshotWatcher) TakeSnapshot() ([]byte, error) {
	sw.C <- struct{}{}
	return nil, nil
}

func TestSnapshot(t *testing.T) {
	prevDefaultSnapshotCount := defaultSnapshotCount
	prevSnapshotCatchUpEntriesN := snapshotCatchUpEntriesN
	defaultSnapshotCount = 4
	snapshotCatchUpEntriesN = 4
	defer func() {
		defaultSnapshotCount = prevDefaultSnapshotCount
		snapshotCatchUpEntriesN = prevSnapshotCatchUpEntriesN
	}()

	sw := snapshotWatcher{
		nullFSM: nullFSM{1},
		C:       make(chan struct{}),
	}

	clus := newCluster(sw, nullFSM{2}, nullFSM{3})
	defer clus.closeNoErrors(t)

	go func() {
		clus.peers[0].proposeC <- "foo"
	}()

	c := <-clus.peers[0].node.commitC

	select {
	case <-sw.C:
		t.Fatalf("snapshot triggered before applying done")
	default:
	}
	close(c.applyDoneC)
	<-sw.C
}
