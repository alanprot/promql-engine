package aggregate

import (
	"container/heap"
	"context"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/thanos-community/promql-engine/execution/model"
	"golang.org/x/exp/slices"
)

type kAggregate struct {
	next    model.VectorOperator
	paramOp model.VectorOperator

	vectorPool *model.VectorPool

	by          bool
	labels      []string
	aggregation parser.ItemType

	once        sync.Once
	series      []labels.Labels
	inputToHeap []*samplesHeap
	heaps       []*samplesHeap
	compare     func(float64, float64) bool
}

func NewKHashAggregate(
	points *model.VectorPool,
	next model.VectorOperator,
	paramOp model.VectorOperator,
	aggregation parser.ItemType,
	by bool,
	labels []string,
) (model.VectorOperator, error) {
	var compare func(float64, float64) bool

	if aggregation == parser.TOPK {
		compare = func(f float64, s float64) bool {
			return f < s
		}
	} else {
		compare = func(f float64, s float64) bool {
			return s < f
		}
	}
	// Grouping labels need to be sorted in order for metric hashing to work.
	// https://github.com/prometheus/prometheus/blob/8ed39fdab1ead382a354e45ded999eb3610f8d5f/model/labels/labels.go#L162-L181
	slices.Sort(labels)

	a := &kAggregate{
		next:        next,
		vectorPool:  points,
		by:          by,
		aggregation: aggregation,
		labels:      labels,
		paramOp:     paramOp,
		compare:     compare,
	}

	return a, nil
}

func (a *kAggregate) Next(ctx context.Context) ([]model.StepVector, error) {
	in, err := a.next.Next(ctx)
	if err != nil {
		return nil, err
	}
	if in == nil {
		return nil, nil
	}

	defer a.next.GetPool().PutVectors(in)

	var arg float64

	if a.paramOp != nil {
		p, err := a.paramOp.Next(ctx)
		if err != nil {
			return nil, err
		}
		arg = p[0].Samples[0]
	}

	a.once.Do(func() { err = a.init(ctx) })
	if err != nil {
		return nil, err
	}

	result := a.vectorPool.GetVectorBatch()
	for _, vector := range in {
		a.aggregate(vector.T, &result, int(arg), vector.SampleIDs, vector.Samples)
		a.next.GetPool().PutStepVector(vector)
	}

	return result, nil
}

func (a *kAggregate) Series(ctx context.Context) ([]labels.Labels, error) {
	var err error
	a.once.Do(func() { err = a.init(ctx) })
	if err != nil {
		return nil, err
	}

	return a.series, nil
}

func (a *kAggregate) GetPool() *model.VectorPool {
	return a.vectorPool
}

func (a *kAggregate) Explain() (me string, next []model.VectorOperator) {
	if a.by {
		return fmt.Sprintf("[*kaggregate] %v by (%v)", a.aggregation.String(), a.labels), []model.VectorOperator{a.paramOp, a.next}
	}
	return fmt.Sprintf("[*kaggregate] %v without (%v)", a.aggregation.String(), a.labels), []model.VectorOperator{a.paramOp, a.next}
}

func (a *kAggregate) init(ctx context.Context) error {
	series, err := a.next.Series(ctx)
	if err != nil {
		return err
	}
	hapsHash := make(map[uint64]*samplesHeap)
	buf := make([]byte, 1024)
	for i := 0; i < len(series); i++ {
		hash, _, _ := hashMetric(series[i], !a.by, a.labels, buf)
		h, ok := hapsHash[hash]
		if !ok {
			h = &samplesHeap{compare: a.compare}
			hapsHash[hash] = h
			a.heaps = append(a.heaps, h)
		}
		a.inputToHeap = append(a.inputToHeap, h)
	}
	a.vectorPool.SetStepSize(len(series))
	a.series = series
	return nil
}

func (a *kAggregate) aggregate(t int64, result *[]model.StepVector, k int, SampleIDs []uint64, samples []float64) {
	for i, sId := range SampleIDs {
		h := a.inputToHeap[sId]
		if h.Len() < k || h.compare(h.entries[0].total, samples[i]) || math.IsNaN(h.entries[0].total) {
			if k == 1 && h.Len() == 1 {
				h.entries[0].sId = sId
				h.entries[0].total = samples[i]
				continue
			}

			if h.Len() == k {
				heap.Pop(h)
			}

			heap.Push(h, &entry{sId: sId, total: samples[i]})
		}
	}

	for _, h := range a.heaps {
		s := a.vectorPool.GetStepVector(t)
		// The heap keeps the lowest value on top, so reverse it.
		if len(h.entries) > 1 {
			sort.Sort(sort.Reverse(h))
		}

		for _, e := range h.entries {
			s.SampleIDs = append(s.SampleIDs, e.sId)
			s.Samples = append(s.Samples, e.total)
		}
		*result = append(*result, s)
		h.entries = h.entries[:0]
	}
}

type entry struct {
	sId   uint64
	total float64
}

type samplesHeap struct {
	entries []entry
	compare func(float64, float64) bool
}

func (s samplesHeap) Len() int {
	return len(s.entries)
}

func (s samplesHeap) Less(i, j int) bool {
	if math.IsNaN(s.entries[i].total) {
		return true
	}
	return s.compare(s.entries[i].total, s.entries[j].total)
}

func (s samplesHeap) Swap(i, j int) {
	s.entries[i], s.entries[j] = s.entries[j], s.entries[i]
}

func (s *samplesHeap) Push(x interface{}) {
	s.entries = append(s.entries, *(x.(*entry)))
}

func (s *samplesHeap) Pop() interface{} {
	old := (*s).entries
	n := len(old)
	el := old[n-1]
	(*s).entries = old[0 : n-1]
	return el
}
