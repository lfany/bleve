//  Copyright (c) 2017 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 		http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package scorch

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/blevesearch/bleve/analysis"
	"github.com/blevesearch/bleve/document"
	"github.com/blevesearch/bleve/index"
	"github.com/blevesearch/bleve/index/scorch/segment"
	"github.com/blevesearch/bleve/index/scorch/segment/mem"
	"github.com/blevesearch/bleve/index/store"
	"github.com/blevesearch/bleve/registry"
	"github.com/boltdb/bolt"
)

const Name = "scorch"

const Version uint8 = 1

// UnInvertIndex is implemented by various scorch index implementations
// to provide the un inverting of the postings or other indexed values.
type UnInvertIndex interface {
	// apparently need better namings here..
	VisitDocumentFieldTerms(localDocNum uint64, fields []string,
		visitor index.DocumentFieldTermVisitor) error
}

type Scorch struct {
	readOnly      bool
	version       uint8
	config        map[string]interface{}
	analysisQueue *index.AnalysisQueue
	stats         *Stats
	nextSegmentID uint64
	path          string

	unsafeBatch bool

	rootLock             sync.RWMutex
	root                 *IndexSnapshot // holds 1 ref-count on the root
	rootPersisted        []chan error   // closed when root is persisted
	nextSnapshotEpoch    uint64
	eligibleForRemoval   []uint64        // Index snapshot epochs that are safe to GC.
	ineligibleForRemoval map[string]bool // Filenames that should not be GC'ed yet.

	closeCh            chan struct{}
	introductions      chan *segmentIntroduction
	merges             chan *segmentMerge
	introducerNotifier chan *epochWatcher
	revertToSnapshots  chan *snapshotReversion
	persisterNotifier  chan notificationChan
	rootBolt           *bolt.DB
	asyncTasks         sync.WaitGroup

	onEvent func(event Event)
}

// Event represents the information provided in an OnEvent() callback.
type Event struct {
	Kind     EventKind
	Scorch   *Scorch
	Duration time.Duration
}

// EventKind represents an event code for OnEvent() callbacks.
type EventKind int

// EventKindCLoseStart is fired when a Scorch.Close() has begun.
var EventKindCloseStart = EventKind(1)

// EventKindClose is fired when a scorch index has been fully closed.
var EventKindClose = EventKind(2)

// EventKindMergerProgress is fired when the merger has completed a
// round of merge processing.
var EventKindMergerProgress = EventKind(3)

// EventKindPersisterProgress is fired when the persister has completed
// a round of persistence processing.
var EventKindPersisterProgress = EventKind(4)

// EventKindBatchIntroductionStart is fired when Batch() is invoked which
// introduces a new segment.
var EventKindBatchIntroductionStart = EventKind(5)

// EventKindBatchIntroduction is fired when Batch() completes.
var EventKindBatchIntroduction = EventKind(6)

func NewScorch(storeName string,
	config map[string]interface{},
	analysisQueue *index.AnalysisQueue) (index.Index, error) {
	rv := &Scorch{
		version:              Version,
		config:               config,
		analysisQueue:        analysisQueue,
		stats:                &Stats{},
		nextSnapshotEpoch:    1,
		closeCh:              make(chan struct{}),
		ineligibleForRemoval: map[string]bool{},
	}
	rv.root = &IndexSnapshot{parent: rv, refs: 1}
	ro, ok := config["read_only"].(bool)
	if ok {
		rv.readOnly = ro
	}
	ub, ok := config["unsafe_batch"].(bool)
	if ok {
		rv.unsafeBatch = ub
	}
	return rv, nil
}

func (s *Scorch) SetEventCallback(f func(Event)) {
	s.onEvent = f
}

func (s *Scorch) fireEvent(kind EventKind, dur time.Duration) {
	if s.onEvent != nil {
		s.onEvent(Event{Kind: kind, Scorch: s, Duration: dur})
	}
}

func (s *Scorch) Open() error {
	var ok bool
	s.path, ok = s.config["path"].(string)
	if !ok {
		return fmt.Errorf("must specify path")
	}
	if s.path == "" {
		s.unsafeBatch = true
	}

	var rootBoltOpt *bolt.Options
	if s.readOnly {
		rootBoltOpt = &bolt.Options{
			ReadOnly: true,
		}
	} else {
		if s.path != "" {
			err := os.MkdirAll(s.path, 0700)
			if err != nil {
				return err
			}
		}
	}
	rootBoltPath := s.path + string(os.PathSeparator) + "root.bolt"
	var err error
	if s.path != "" {
		s.rootBolt, err = bolt.Open(rootBoltPath, 0600, rootBoltOpt)
		if err != nil {
			return err
		}

		// now see if there is any existing state to load
		err = s.loadFromBolt()
		if err != nil {
			_ = s.Close()
			return err
		}
	}

	s.introductions = make(chan *segmentIntroduction)
	s.merges = make(chan *segmentMerge)
	s.introducerNotifier = make(chan *epochWatcher, 1)
	s.revertToSnapshots = make(chan *snapshotReversion)
	s.persisterNotifier = make(chan notificationChan)

	if !s.readOnly && s.path != "" {
		err := s.removeOldZapFiles() // Before persister or merger create any new files.
		if err != nil {
			_ = s.Close()
			return err
		}
	}

	s.asyncTasks.Add(1)
	go s.mainLoop()

	if !s.readOnly && s.path != "" {
		s.asyncTasks.Add(1)
		go s.persisterLoop()
		s.asyncTasks.Add(1)
		go s.mergerLoop()
	}

	return nil
}

func (s *Scorch) Close() (err error) {
	startTime := time.Now()
	defer func() {
		s.fireEvent(EventKindClose, time.Since(startTime))
	}()

	s.fireEvent(EventKindCloseStart, 0)

	// signal to async tasks we want to close
	close(s.closeCh)
	// wait for them to close
	s.asyncTasks.Wait()
	// now close the root bolt
	if s.rootBolt != nil {
		err = s.rootBolt.Close()
		s.rootLock.Lock()
		if s.root != nil {
			_ = s.root.DecRef()
		}
		s.root = nil
		s.rootLock.Unlock()
	}

	return
}

func (s *Scorch) Update(doc *document.Document) error {
	b := index.NewBatch()
	b.Update(doc)
	return s.Batch(b)
}

func (s *Scorch) Delete(id string) error {
	b := index.NewBatch()
	b.Delete(id)
	return s.Batch(b)
}

// Batch applices a batch of changes to the index atomically
func (s *Scorch) Batch(batch *index.Batch) error {
	start := time.Now()

	defer func() {
		s.fireEvent(EventKindBatchIntroduction, time.Since(start))
	}()

	resultChan := make(chan *index.AnalysisResult, len(batch.IndexOps))

	var numUpdates uint64
	var numDeletes uint64
	var numPlainTextBytes uint64
	var ids []string
	for docID, doc := range batch.IndexOps {
		if doc != nil {
			// insert _id field
			doc.AddField(document.NewTextFieldCustom("_id", nil, []byte(doc.ID), document.IndexField|document.StoreField, nil))
			numUpdates++
			numPlainTextBytes += doc.NumPlainTextBytes()
		} else {
			numDeletes++
		}
		ids = append(ids, docID)
	}

	// FIXME could sort ids list concurrent with analysis?

	go func() {
		for _, doc := range batch.IndexOps {
			if doc != nil {
				aw := index.NewAnalysisWork(s, doc, resultChan)
				// put the work on the queue
				s.analysisQueue.Queue(aw)
			}
		}
	}()

	// wait for analysis result
	analysisResults := make([]*index.AnalysisResult, int(numUpdates))
	var itemsDeQueued uint64
	for itemsDeQueued < numUpdates {
		result := <-resultChan
		analysisResults[itemsDeQueued] = result
		itemsDeQueued++
	}
	close(resultChan)

	atomic.AddUint64(&s.stats.analysisTime, uint64(time.Since(start)))

	// notify handlers that we're about to introduce a segment
	s.fireEvent(EventKindBatchIntroductionStart, 0)

	var newSegment segment.Segment
	if len(analysisResults) > 0 {
		newSegment = mem.NewFromAnalyzedDocs(analysisResults)
	}

	err := s.prepareSegment(newSegment, ids, batch.InternalOps)
	if err != nil {
		if newSegment != nil {
			_ = newSegment.Close()
		}
		atomic.AddUint64(&s.stats.errors, 1)
	} else {
		atomic.AddUint64(&s.stats.updates, numUpdates)
		atomic.AddUint64(&s.stats.deletes, numDeletes)
		atomic.AddUint64(&s.stats.batches, 1)
		atomic.AddUint64(&s.stats.numPlainTextBytesIndexed, numPlainTextBytes)
	}
	return err
}

func (s *Scorch) prepareSegment(newSegment segment.Segment, ids []string,
	internalOps map[string][]byte) error {

	// new introduction
	introduction := &segmentIntroduction{
		id:        atomic.AddUint64(&s.nextSegmentID, 1),
		data:      newSegment,
		ids:       ids,
		obsoletes: make(map[uint64]*roaring.Bitmap),
		internal:  internalOps,
		applied:   make(chan error),
	}

	if !s.unsafeBatch {
		introduction.persisted = make(chan error, 1)
	}

	// get read lock, to optimistically prepare obsoleted info
	s.rootLock.RLock()
	for _, seg := range s.root.segment {
		delta, err := seg.segment.DocNumbers(ids)
		if err != nil {
			s.rootLock.RUnlock()
			return err
		}
		introduction.obsoletes[seg.id] = delta
	}
	s.rootLock.RUnlock()

	s.introductions <- introduction

	// block until this segment is applied
	err := <-introduction.applied
	if err != nil {
		return err
	}

	if introduction.persisted != nil {
		err = <-introduction.persisted
	}

	return err
}

func (s *Scorch) SetInternal(key, val []byte) error {
	b := index.NewBatch()
	b.SetInternal(key, val)
	return s.Batch(b)
}

func (s *Scorch) DeleteInternal(key []byte) error {
	b := index.NewBatch()
	b.DeleteInternal(key)
	return s.Batch(b)
}

// Reader returns a low-level accessor on the index data. Close it to
// release associated resources.
func (s *Scorch) Reader() (index.IndexReader, error) {
	s.rootLock.RLock()
	rv := &Reader{root: s.root}
	rv.root.AddRef()
	s.rootLock.RUnlock()
	return rv, nil
}

func (s *Scorch) Stats() json.Marshaler {
	return s.stats
}
func (s *Scorch) StatsMap() map[string]interface{} {
	return s.stats.statsMap()
}

func (s *Scorch) Analyze(d *document.Document) *index.AnalysisResult {
	rv := &index.AnalysisResult{
		Document: d,
		Analyzed: make([]analysis.TokenFrequencies, len(d.Fields)+len(d.CompositeFields)),
		Length:   make([]int, len(d.Fields)+len(d.CompositeFields)),
	}

	for i, field := range d.Fields {
		if field.Options().IsIndexed() {
			fieldLength, tokenFreqs := field.Analyze()
			rv.Analyzed[i] = tokenFreqs
			rv.Length[i] = fieldLength

			if len(d.CompositeFields) > 0 {
				// see if any of the composite fields need this
				for _, compositeField := range d.CompositeFields {
					compositeField.Compose(field.Name(), fieldLength, tokenFreqs)
				}
			}
		}
	}

	return rv
}

func (s *Scorch) Advanced() (store.KVStore, error) {
	return nil, nil
}

func (s *Scorch) AddEligibleForRemoval(epoch uint64) {
	s.rootLock.Lock()
	if s.root == nil || s.root.epoch != epoch {
		s.eligibleForRemoval = append(s.eligibleForRemoval, epoch)
	}
	s.rootLock.Unlock()
}

func (s *Scorch) MemoryUsed() uint64 {
	var memUsed uint64
	s.rootLock.RLock()
	for _, segmentSnapshot := range s.root.segment {
		memUsed += 8 /* size of id -> uint64 */ +
			segmentSnapshot.segment.SizeInBytes()
		if segmentSnapshot.deleted != nil {
			memUsed += segmentSnapshot.deleted.GetSizeInBytes()
		}
		memUsed += segmentSnapshot.cachedDocs.sizeInBytes()
	}
	s.rootLock.RUnlock()
	return memUsed
}

func (s *Scorch) markIneligibleForRemoval(filename string) {
	s.rootLock.Lock()
	s.ineligibleForRemoval[filename] = true
	s.rootLock.Unlock()
}

func (s *Scorch) unmarkIneligibleForRemoval(filename string) {
	s.rootLock.Lock()
	delete(s.ineligibleForRemoval, filename)
	s.rootLock.Unlock()
}

func init() {
	registry.RegisterIndexType(Name, NewScorch)
}