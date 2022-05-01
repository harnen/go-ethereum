// Copyright 2022 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package discover

import (
	"context"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/p2p/discover/topicindex"
	"github.com/ethereum/go-ethereum/p2p/discover/v5wire"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// topicSystem manages the resources required for registering and searching
// in topics.
type topicSystem struct {
	transport *UDPv5
	config    topicindex.Config

	mu     sync.Mutex
	reg    map[topicindex.TopicID]*topicReg
	search map[topicindex.TopicID]*topicSearch
}

func newTopicSystem(transport *UDPv5, config topicindex.Config) *topicSystem {
	return &topicSystem{
		transport: transport,
		config:    config,
		reg:       make(map[topicindex.TopicID]*topicReg),
		search:    make(map[topicindex.TopicID]*topicSearch),
	}
}

func (sys *topicSystem) register(topic topicindex.TopicID) {
	sys.mu.Lock()
	defer sys.mu.Unlock()

	if _, ok := sys.reg[topic]; ok {
		return
	}
	sys.reg[topic] = newTopicReg(sys, topic)
}

func (sys *topicSystem) stopRegister(topic topicindex.TopicID) {
	sys.mu.Lock()
	defer sys.mu.Unlock()

	if reg := sys.reg[topic]; reg != nil {
		reg.stop()
		delete(sys.reg, topic)
	}
}

func (sys *topicSystem) stop() {
	sys.mu.Lock()
	defer sys.mu.Unlock()

	for topic, reg := range sys.reg {
		reg.stop()
		delete(sys.reg, topic)
	}
	for topic, s := range sys.search {
		s.stop()
		delete(sys.search, topic)
	}
}

func (sys *topicSystem) newSearchIterator(topic topicindex.TopicID) enode.Iterator {
	sys.mu.Lock()
	defer sys.mu.Unlock()

	s, _ := sys.search[topic]
	if s == nil {
		s = newTopicSearch(sys, topic)
		sys.search[topic] = s
	}
	s.refcount++
	return newTopicSearchIterator(sys, s)
}

func (sys *topicSystem) iteratorClosed(s *topicSearch) {
	sys.mu.Lock()
	defer sys.mu.Unlock()

	s.refcount--
	if s.refcount == 0 {
		// Last iterator of this search was closed.
		s.stop()
	}
}

// topicReg handles registering for a single topic.
type topicReg struct {
	state *topicindex.Registration
	clock mclock.Clock
	wg    sync.WaitGroup
	quit  chan struct{}

	lookupCtx     context.Context
	lookupCancel  context.CancelFunc
	lookupTarget  chan enode.ID
	lookupResults chan []*enode.Node

	regRequest  chan *topicindex.RegAttempt
	regResponse chan regResponse
}

type regResponse struct {
	att *topicindex.RegAttempt
	msg *v5wire.Regconfirmation
	err error
}

func newTopicReg(sys *topicSystem, topic topicindex.TopicID) *topicReg {
	ctx, cancel := context.WithCancel(context.Background())
	reg := &topicReg{
		state:         topicindex.NewRegistration(topic, sys.config),
		clock:         sys.config.Clock,
		quit:          make(chan struct{}),
		lookupCtx:     ctx,
		lookupCancel:  cancel,
		lookupTarget:  make(chan enode.ID),
		lookupResults: make(chan []*enode.Node, 1),
		regRequest:    make(chan *topicindex.RegAttempt),
		regResponse:   make(chan regResponse),
	}
	reg.wg.Add(3)
	go reg.run(sys)
	go reg.runLookups(sys)
	go reg.runRequests(sys)
	return reg
}

func (reg *topicReg) stop() {
	close(reg.quit)
	reg.wg.Wait()
}

func (reg *topicReg) run(sys *topicSystem) {
	defer reg.wg.Done()

	var (
		updateEv      = mclock.NewAlarm(reg.clock)
		updateCh      <-chan struct{}
		sendAttempt   *topicindex.RegAttempt
		sendAttemptCh chan<- *topicindex.RegAttempt
	)

	for {
		// Disable updates while dispatching the next attempt's request.
		if sendAttempt == nil {
			next := reg.state.NextUpdateTime()
			if next != topicindex.Never {
				updateEv.Schedule(next)
				updateCh = updateEv.C()
			}
		}

		select {
		// Loop exit.
		case <-reg.quit:
			close(reg.regRequest)
			close(reg.lookupTarget)
			reg.lookupCancel()
			return

		// Lookup management.
		case reg.lookupTarget <- reg.state.LookupTarget():
		case nodes := <-reg.lookupResults:
			reg.state.AddNodes(nodes)

		// Attempt queue updates.
		case <-updateCh:
			att := reg.state.Update()
			if att != nil {
				sendAttempt = att
				sendAttemptCh = reg.regRequest
			}

		// Registration requests.
		case sendAttemptCh <- sendAttempt:
			reg.state.StartRequest(sendAttempt)
			sendAttemptCh = nil
			sendAttempt = nil

		case resp := <-reg.regResponse:
			if resp.err != nil {
				reg.state.HandleErrorResponse(resp.att, resp.err)
				continue
			}
			msg := resp.msg
			// TODO: handle overflow
			wt := time.Duration(msg.WaitTime) * time.Second
			if len(msg.Ticket) > 0 {
				reg.state.HandleTicketResponse(resp.att, msg.Ticket, wt)
			} else {
				reg.state.HandleRegistered(resp.att, wt, 10*time.Minute)
			}
		}
	}
}

func (reg *topicReg) runLookups(sys *topicSystem) {
	defer reg.wg.Done()

	for target := range reg.lookupTarget {
		l := sys.transport.newLookup(reg.lookupCtx, target)
		for l.advance() {
			// Send results of this step over to the main loop.
			nodes := unwrapNodes(l.replyBuffer)
			select {
			case reg.lookupResults <- nodes:
			case <-reg.lookupCtx.Done():
				return
			}
		}

		// Wait a bit before starting the next lookup.
		reg.sleep(2 * time.Second)
	}
}

func (reg *topicReg) sleep(d time.Duration) {
	sleep := reg.clock.NewTimer(d)
	defer sleep.Stop()
	select {
	case <-sleep.C():
	case <-reg.quit:
	}
}

// runRequests performs topic registration requests.
// TODO: this is not great because it sends one at a time and waits for a response.
// registrations could just be sent async.
func (reg *topicReg) runRequests(sys *topicSystem) {
	defer reg.wg.Done()

	for attempt := range reg.regRequest {
		n := attempt.Node
		topic := reg.state.Topic()
		resp := regResponse{att: attempt}
		resp.msg, resp.err = sys.transport.topicRegister(n, topic, attempt.Ticket)

		// Send response to main loop.
		select {
		case reg.regResponse <- resp:
		case <-reg.quit:
			return
		}
	}
}

// topicSearch handles searching in a single topic.
type topicSearch struct {
	state       *topicindex.Search
	clock       mclock.Clock
	resultScope event.SubscriptionScope
	resultFeed  event.Feed

	wg   sync.WaitGroup
	quit chan struct{}

	queryCh     chan *enode.Node
	queryRespCh chan topicQueryResp

	lookupCtx     context.Context
	lookupCancel  context.CancelFunc
	lookupTarget  chan enode.ID
	lookupResults chan []*enode.Node

	// This tracks how many iterators are subscribed to this search.
	// Access to this field is guarded by topicSystem.mu.
	refcount int
}

type topicQueryResp struct {
	src   *enode.Node
	nodes []*enode.Node
	err   error
}

func newTopicSearch(sys *topicSystem, topic topicindex.TopicID) *topicSearch {
	ctx, cancel := context.WithCancel(context.Background())

	s := &topicSearch{
		state: topicindex.NewSearch(topic, sys.config),
		clock: sys.config.Clock,
		quit:  make(chan struct{}),

		// query
		queryCh:     make(chan *enode.Node),
		queryRespCh: make(chan topicQueryResp),

		// lookup
		lookupCtx:     ctx,
		lookupCancel:  cancel,
		lookupTarget:  make(chan enode.ID),
		lookupResults: make(chan []*enode.Node, 1),
	}
	s.wg.Add(3)
	go s.run()
	go s.runLookups(sys)
	go s.runRequests(sys)
	return s
}

func (s *topicSearch) subscribeResults(ch chan<- *enode.Node) event.Subscription {
	return s.resultScope.Track(s.resultFeed.Subscribe(ch))
}

func (s *topicSearch) stop() {
	close(s.quit)
	s.wg.Wait()
}

func (s *topicSearch) run() {
	defer s.wg.Done()

	lookupEv := mclock.NewAlarm(s.clock)

	for {
		next := s.state.NextLookupTime()
		if next != topicindex.Never {
			lookupEv.Schedule(next)
		}

		select {
		// Loop exit.
		case <-s.quit:
			s.lookupCancel()
			close(s.queryCh)
			s.resultScope.Close()
			return

		// Lookup management.
		case <-lookupEv.C():
			select {
			case s.lookupTarget <- s.state.LookupTarget():
			case <-s.quit:
			}

		case nodes := <-s.lookupResults:
			s.state.AddNodes(nodes)

		// Queries.
		// case <-queryEv:

		case resp := <-s.queryRespCh:
			for n := range resp.nodes {
				s.resultFeed.Send(n)
			}
		}
	}
}

// TODO: this should be shared with topicReg somehow.
func (s *topicSearch) runLookups(sys *topicSystem) {
	defer s.wg.Done()

	for target := range s.lookupTarget {
		l := sys.transport.newLookup(s.lookupCtx, target)
		// Note: here, only the final results (i.e. closest to target) are taken.
		nodes := l.run()
		select {
		case s.lookupResults <- nodes:
		case <-s.lookupCtx.Done():
			return
		}
	}
}

func (s *topicSearch) runRequests(sys *topicSystem) {
	defer s.wg.Done()

	for n := range s.queryCh {
		topic := s.state.Topic()
		resp := topicQueryResp{}
		resp.nodes, resp.err = sys.transport.topicQuery(n, topic)

		// Send response to main loop.
		select {
		case s.queryRespCh <- resp:
		case <-s.quit:
			return
		}
	}
}

// topicSearchIterator implements enode.Iterator. It is an iterator
// that returns nodes found by topic search.
type topicSearchIterator struct {
	sys    *topicSystem
	search *topicSearch

	sub     event.Subscription
	ch      chan *enode.Node
	closing sync.Once

	node *enode.Node
}

func newTopicSearchIterator(sys *topicSystem, search *topicSearch) *topicSearchIterator {
	ch := make(chan *enode.Node, 200)
	sub := search.subscribeResults(ch)
	return &topicSearchIterator{sys: sys, search: search, sub: sub, ch: ch}
}

func (tsi *topicSearchIterator) Next() bool {
	select {
	case n, ok := <-tsi.ch:
		tsi.node = n
		return ok
	case <-tsi.sub.Err():
		// This case activates when topicSearch is stopped.
		tsi.Close()
		return false
	}
}

func (tsi *topicSearchIterator) Node() *enode.Node {
	return tsi.node
}

func (tsi *topicSearchIterator) Close() {
	tsi.closing.Do(func() {
		tsi.sys.iteratorClosed(tsi.search)
		tsi.sub.Unsubscribe()
		close(tsi.ch)

		// Drain ch. This guarantees that, when Close is done,
		// Next will always return false.
		for range tsi.ch {
		}
	})
}
