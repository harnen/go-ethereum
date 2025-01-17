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
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
)

const (
	// searchTableDepth is the number of buckets kept in the search table.
	//
	// The table only keeps nodes at logdist(topic, n) > (256 - searchTableDepth).
	// Should there be any nodes which are closer than this, they just go into the last
	// (closest) bucket.
	searchTableDepth = 40

	// This defines the minimum delay between two lookups started by Search.
	searchLookupMinDelay = 3 * time.Second
)

// Search is the state associated with searching for a single topic.
type Search struct {
	topic TopicID
	cfg   Config

	// Note: search buckets are ordered far -> close.
	buckets [searchTableDepth]searchBucket

	resultBuffer []*enode.Node
	numResults   int

	queriesWithoutNewNodes int
}

type searchBucket struct {
	dist       int
	new        map[enode.ID]*enode.Node
	asked      map[enode.ID]struct{}
	numResults int
}

// NewSearch creates a new topic search state.
func NewSearch(topic TopicID, config Config) *Search {
	config = config.withDefaults()
	s := &Search{cfg: config, topic: topic}
	dist := 256
	for i := range s.buckets {
		s.buckets[i].new = make(map[enode.ID]*enode.Node)
		s.buckets[i].asked = make(map[enode.ID]struct{})
		s.buckets[i].dist = dist
		dist--
	}
	return s
}

// IsDone reports whether the search table is saturated. When it returns true,
// this search state should be abandoned and a new search started using a
// fresh Search instance.
func (s *Search) IsDone() bool {
	// TODO: what's the condition here?
	//
	// Ideas:
	//
	//   - n total results reached
	//   - results from n sources received
	//   - closest nodes reached (requires improved lookup tracking)
	//   - buckets fuller than X

	// The search cannot be done while there are unused results in the buffer,
	// or while there are still nodes that could be asked.
	if len(s.resultBuffer) > 0 {
		return false
	}
	for _, b := range s.buckets {
		if len(b.new) > 0 {
			return false
		}
	}
	// No unasked nodes remain. Consider it done when the last
	// two lookups didn't yield any new nodes.
	return s.queriesWithoutNewNodes >= 2
}

// AddNodes adds the results of a lookup to the table.
func (s *Search) AddNodes(src *enode.Node, nodes []*enode.Node) {
	var anyNewNode bool
	for _, n := range nodes {
		if n.ID() == s.cfg.Self {
			continue
		}
		b := s.bucket(n.ID())
		if !b.contains(n.ID()) {
			anyNewNode = true
		}
		if b.count() < s.cfg.SearchBucketSize {
			b.add(n)
		}
	}

	if !anyNewNode {
		s.queriesWithoutNewNodes++
	} else {
		s.queriesWithoutNewNodes = 0
	}
}

// QueryTarget returns a random node to which a topic query should be sent.
func (s *Search) QueryTarget() *enode.Node {
	for _, b := range s.buckets {
		for _, n := range b.new {
			return n
		}
	}
	return nil
}

// AddQueryResults adds the response nodes for a topic query to the table.
func (s *Search) AddQueryResults(from *enode.Node, results []*enode.Node) {
	b := s.bucket(from.ID())
	b.setAsked(from)

	for _, n := range results {
		if n.ID() == s.cfg.Self {
			continue
		}
		s.cfg.Log.Debug("Added topic search result", "topic", s.topic, "fromid", from.ID(), "rid", n.ID())
		b.numResults++
		s.numResults++
		s.resultBuffer = append(s.resultBuffer, n)
	}
}

// PeekResult returns a node from the result set.
// When no result is available, it returns nil.
func (s *Search) PeekResult() *enode.Node {
	if len(s.resultBuffer) > 0 {
		return s.resultBuffer[0]
	}
	return nil
}

// PopResult removes a result node.
func (s *Search) PopResult() {
	if len(s.resultBuffer) == 0 {
		panic("PopResult with len(results) == 0")
	}
	s.resultBuffer = append(s.resultBuffer[:0], s.resultBuffer[1:]...)
}

func (s *Search) bucket(id enode.ID) *searchBucket {
	dist := 256 - enode.LogDist(enode.ID(s.topic), id)
	if dist > len(s.buckets)-1 {
		dist = len(s.buckets) - 1
	}
	return &s.buckets[dist]
}

func (b *searchBucket) contains(id enode.ID) bool {
	_, inNew := b.new[id]
	_, inAsked := b.asked[id]
	return inNew || inAsked
}

func (b *searchBucket) count() int {
	return len(b.new) + len(b.asked)
}

func (b *searchBucket) add(n *enode.Node) {
	id := n.ID()
	if _, inAsked := b.asked[id]; inAsked {
		return
	}
	b.new[id] = newer(b.new[id], n)
}

func (b *searchBucket) setAsked(n *enode.Node) {
	b.asked[n.ID()] = struct{}{}
	delete(b.new, n.ID())
}

func newer(n1 *enode.Node, n2 *enode.Node) *enode.Node {
	switch {
	case n1 == nil:
		return n2
	case n2 == nil:
		return n1
	case n1.Seq() >= n2.Seq():
		return n1
	default:
		return n2
	}
}
