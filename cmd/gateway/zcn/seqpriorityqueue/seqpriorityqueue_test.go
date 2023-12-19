package seqpriorityqueue

import (
	"testing"
	"time"
)

func TestSeqPriorityQueue(t *testing.T) {
	pq := NewSeqPriorityQueue()

	go func() {
		for i := 1; i <= 5; i++ {
			pq.Push(i)
			time.Sleep(100 * time.Millisecond)
		}
	}()

	for i := 1; i <= 5; i++ {
		if got := pq.Popup(); got != i {
			t.Errorf("Popup() = %v, want %v", got, i)
		}
	}
}

func TestSeqPriorityQueueOutOfOrder(t *testing.T) {
	pq := NewSeqPriorityQueue()

	go func() {
		for _, i := range []int{5, 1, 3, 2, 4} {
			pq.Push(i)
			time.Sleep(100 * time.Millisecond)
		}
	}()

	for i := 1; i <= 5; i++ {
		if got := pq.Popup(); got != i {
			t.Errorf("Popup() = %v, want %v", got, i)
		}
	}
}

func TestSeqPriorityQueueEmpty(t *testing.T) {
	pq := NewSeqPriorityQueue()

	go func() {
		time.Sleep(500 * time.Millisecond)
		pq.Push(1)
	}()

	if got := pq.Popup(); got != 1 {
		t.Errorf("Popup() = %v, want %v", got, 1)
	}
}
func TestSeqPriorityQueueDone(t *testing.T) {
	pq := NewSeqPriorityQueue()

	go func() {
		for _, i := range []int{1, 2, 3} {
			pq.Push(i)
			time.Sleep(100 * time.Millisecond)
		}
		pq.Done()
	}()

	for i := 1; i <= 3; i++ {
		if got := pq.Popup(); got != i {
			t.Errorf("Popup() = %v, want %v", got, i)
		}
	}

	if got := pq.Popup(); got != -1 {
		t.Errorf("Popup() after Done() = %v, want %v", got, -1)
	}
}
