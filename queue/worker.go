package queue

import (
	"sync"
)

type WorkerPool struct {
	workers chan struct{}
	wg      sync.WaitGroup
}

func NewWorkerPool(size int) *WorkerPool {
	return &WorkerPool{
		workers: make(chan struct{}, size),
	}
}

func (p *WorkerPool) Submit(task func()) {
	p.wg.Add(1)
	p.workers <- struct{}{}

	go func() {
		defer func() {
			<-p.workers
			p.wg.Done()
		}()
		task()
	}()
}

func (p *WorkerPool) Wait() {
	p.wg.Wait()
}

func (p *WorkerPool) Stop() {
	close(p.workers)
}