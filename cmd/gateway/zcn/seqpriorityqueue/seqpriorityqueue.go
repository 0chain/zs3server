package seqpriorityqueue

import (
	"container/heap"
	"sync"
)

type queue []int

func (pq queue) Len() int { return len(pq) }

func (pq queue) Less(i, j int) bool {
	return pq[i] < pq[j]
}

func (pq queue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
}

func (pq *queue) Push(x interface{}) {
	*pq = append(*pq, x.(int))
}

func (pq *queue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	*pq = old[0 : n-1]
	return item
}

// SeqPriorityQueue is a priority queue that pops items in sequential order that starts from 1
type SeqPriorityQueue struct {
	queue queue
	lock  sync.Mutex
	cv    *sync.Cond
	start bool
	next  int
	done  bool
}

// NewSeqPriorityQueue creates a new SequentialPriorityQueue
func NewSeqPriorityQueue() *SeqPriorityQueue {
	pq := &SeqPriorityQueue{
		queue: make(queue, 0),
		start: false,
		next:  1, // Start from 1
		done:  false,
	}
	pq.cv = sync.NewCond(&pq.lock)
	heap.Init(&pq.queue)

	return pq
}

func (pq *SeqPriorityQueue) Push(v int) {
	pq.lock.Lock()
	heap.Push(&pq.queue, v)
	if v == 1 { // Start when 1 is pushed
		pq.start = true
	}
	pq.cv.Signal()
	pq.lock.Unlock()
}

func (pq *SeqPriorityQueue) Done() {
	pq.lock.Lock()
	pq.done = true
	pq.cv.Signal()
	pq.lock.Unlock()
}

func (pq *SeqPriorityQueue) Popup() int {
	pq.lock.Lock()
	for pq.queue.Len() == 0 && !pq.done || !pq.start || (pq.queue.Len() > 0 && pq.queue[0] != pq.next) {
		pq.cv.Wait()
	}
	if pq.queue.Len() == 0 && pq.done {
		pq.lock.Unlock()
		return -1
	}
	item := heap.Pop(&pq.queue).(int)
	pq.next++
	pq.lock.Unlock()
	return item
}
