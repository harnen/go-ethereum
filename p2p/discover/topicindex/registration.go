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

package topicindex

import (
	"container/heap"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

const (
	regBucketMaxReplacements = 20

	// regTableDepth is the number of buckets kept in the registration table.
	//
	// The table only keeps nodes at logdist(topic, n) > (256 - regTableDepth).
	// Should there be any nodes which are closer than this, they just go into the last
	// (closest) bucket.
	regTableDepth = 40
)

// Registration is the state associated with registering in a single topic.
type Registration struct {
	topic TopicID
	cfg   Config
	log   log.Logger

	// Note: registration buckets are ordered close -> far.
	buckets [regTableDepth]regBucket
	heap    regHeap
}

//go:generate go run golang.org/x/tools/cmd/stringer@latest -type RegAttemptState

// RegAttemptState is the state of a registration attempt on a node.
type RegAttemptState int

type regBucket struct {
	dist  int
	att   map[enode.ID]*RegAttempt
	count [nRegStates]int
}

const (
	Standby RegAttemptState = iota
	Waiting
	Registered

	nRegStates = int(Registered) + 1
)

// RegAttempt contains the state of the registration process against
// a single registrar node.
type RegAttempt struct {
	State RegAttemptState

	// NextTime is when the next action related to this attempt must occur.
	//
	// In state 'Waiting'
	//     it is the time of the next registration attempt.
	// In state 'Registered'
	//     it is the time when the ad expires.
	NextTime mclock.AbsTime

	// Node is the registrar node.
	Node *enode.Node

	// Ticket contains the ticket data returned by the last registration call.
	Ticket []byte

	// TotalWaitTime is the time spent waiting so far.
	TotalWaitTime time.Duration

	index  int // index in regHeap
	bucket *regBucket
}

func NewRegistration(topic TopicID, cfg Config) *Registration {
	cfg = cfg.withDefaults()
	r := &Registration{
		topic: topic,
		cfg:   cfg,
		log:   cfg.Log.New("topic", topic),
	}
	dist := 256
	for i := range r.buckets {
		r.buckets[i].att = make(map[enode.ID]*RegAttempt)
		r.buckets[i].dist = dist - (len(r.buckets) - 1) + i
	}
	return r
}

// Topic returns the topic being registered for.
func (r *Registration) Topic() TopicID {
	return r.topic
}

// LookupTarget returns a target that should be walked towards.
func (r *Registration) LookupTarget() enode.ID {
	// This finds the closest bucket with no registrations.
	center := enode.ID(r.topic)
	for _, b := range r.buckets {
		if b.count[Registered] == 0 {
			return enode.RandomID(center, b.dist)
		}
	}
	return center
}

// AddNodes notifies the registration process about found nodes.
func (r *Registration) AddNodes(nodes []*enode.Node) {
	for _, n := range nodes {
		id := n.ID()
		if id == r.cfg.Self {
			continue
		}

		b := r.bucket(id)
		attempt, ok := b.att[id]
		if ok {
			// There is already an attempt scheduled with this node.
			// Update the record if newer.
			if attempt.Node.Seq() < n.Seq() {
				attempt.Node = n
			}
			continue
		}
		// The node is not in the table yet.

		if b.count[Standby] >= regBucketMaxReplacements {
			// There are enough replacements already, ignore the node.
			continue
		}

		// Create a new attempt.
		att := &RegAttempt{Node: n, bucket: b, index: -1}
		b.att[id] = att
		b.count[att.State]++
		r.refillAttempts(att.bucket)
	}
}

func (r *Registration) setAttemptState(att *RegAttempt, state RegAttemptState) {
	att.bucket.count[att.State]--
	att.bucket.count[state]++
	r.log.Trace("Registration attempt state changed", "id", att.Node.ID(), "state", state, "prev", att.State)
	att.State = state
}

// refillAttempts promotes a registrar node from Standby to Waiting.
// This must be called after every potential attempt state change in the bucket.
func (r *Registration) refillAttempts(b *regBucket) {
	if b.count[Waiting] >= r.cfg.RegBucketSize {
		// Enough attempts in state 'Waiting'.
		return
	}

	for _, att := range b.att {
		if att.State == Standby {
			r.setAttemptState(att, Waiting)
			att.NextTime = r.cfg.Clock.Now()
			heap.Push(&r.heap, att)
			break
		}
	}
}

// NextUpdateTime returns the next time Update should be called.
func (r *Registration) NextUpdateTime() mclock.AbsTime {
	for len(r.heap) > 0 {
		att := r.heap[0]
		switch att.State {
		case Standby:
			panic("standby attempt in Registration.heap")
		case Registered, Waiting:
			return att.NextTime
		}
	}
	return Never
}

// Update processes the attempt queue and returns the next attempt in state 'Waiting'.
func (r *Registration) Update() *RegAttempt {
	now := r.cfg.Clock.Now()
	for len(r.heap) > 0 {
		att := r.heap[0]
		switch att.State {
		case Standby:
			panic("standby attempt in Registration.heap")
		case Registered:
			if now >= att.NextTime {
				r.removeAttempt(att)
				r.refillAttempts(att.bucket)
			}
			return nil
		case Waiting:
			if now >= att.NextTime {
				return att
			}
			return nil
		}
	}
	return nil
}

// StartRequest should be called when a registration request is sent for the attempt.
func (r *Registration) StartRequest(att *RegAttempt) {
	if att.State != Waiting {
		panic(fmt.Errorf("StartRequest for attempt with state=%v", att.State))
	}
	heap.Remove(&r.heap, att.index)
	att.index = -2
}

func (r *Registration) validate(att *RegAttempt) {
	if att.index != -2 {
		id := att.Node.ID().Bytes()
		panic(fmt.Errorf("attempt (node %x state %s) has bad index %d", id[:8], att.State, att.index))
	}
}

// HandleTicketResponse should be called when a node responds to a registration
// request with a ticket and waiting time.
func (r *Registration) HandleTicketResponse(att *RegAttempt, ticket []byte, waitTime time.Duration) {
	r.validate(att)
	// TODO
	//    - if wait time > max timeout, remove the attempt
	//    - if number of retries too high, remove

	att.Ticket = ticket
	att.NextTime = r.cfg.Clock.Now().Add(waitTime)
	heap.Push(&r.heap, att)
}

// HandleRegistered should be called when a node confirms topic registration.
func (r *Registration) HandleRegistered(att *RegAttempt, ttl time.Duration) {
	r.validate(att)

	r.log.Trace("Topic registration successful", "id", att.Node.ID())
	r.setAttemptState(att, Registered)
	att.NextTime = r.cfg.Clock.Now().Add(ttl)
	heap.Push(&r.heap, att)

	r.refillAttempts(att.bucket)
}

// HandleErrorResponse should be called when a registration attempt fails.
func (r *Registration) HandleErrorResponse(att *RegAttempt, err error) {
	r.validate(att)

	r.log.Debug("Topic registration failed", "id", att.Node.ID(), "err", err)
	r.removeAttempt(att)
	r.refillAttempts(att.bucket)
}

func (r *Registration) removeAttempt(att *RegAttempt) {
	nid := att.Node.ID()
	if att.bucket.att[nid] != att {
		panic("trying to delete non-existent attempt")
	}
	r.log.Trace("Removing registration attempt", "id", att.Node.ID(), "state", att.State)
	if att.index >= 0 {
		heap.Remove(&r.heap, att.index)
	}
	delete(att.bucket.att, nid)
	att.bucket.count[att.State]--
}

func (r *Registration) bucket(id enode.ID) *regBucket {
	dist := enode.LogDist(enode.ID(r.topic), id)
	index := dist - 256 + (len(r.buckets) - 1)
	if index < 0 {
		index = 0
	}
	return &r.buckets[index]
}

// regHeap is a priority queue of registration attempts. This should not be accessed
// directly. Use heap.Push and heap.Pop to add and remove items.
type regHeap []*RegAttempt

func (rh regHeap) Len() int {
	return len(rh)
}

func (rh regHeap) Less(i, j int) bool {
	return rh[i].NextTime < rh[j].NextTime
}

func (rh regHeap) Swap(i, j int) {
	rh[i], rh[j] = rh[j], rh[i]
	rh[i].index = i
	rh[j].index = j
}

func (rh *regHeap) Push(x interface{}) {
	n := len(*rh)
	item := x.(*RegAttempt)
	item.index = n
	*rh = append(*rh, item)
}

func (rh *regHeap) Pop() interface{} {
	old := *rh
	n := len(old)
	item := old[n-1]

	old[n-1] = nil  // avoid memory leak
	item.index = -1 // for safety
	*rh = old[0 : n-1]

	return item
}
