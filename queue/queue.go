package queue

import (
	"fmt"
	"sync"
	"time"

	"whatsapp-gpt-bot/types"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type MessageBatch struct {
	Type     types.MessageType
	Messages []types.Message
}

type Queue struct {
	messages    chan types.Message
	workerPool  *WorkerPool
	batchSize   int
	batchWindow time.Duration
	batches     map[types.MessageType]*MessageBatch
	batchMutex  sync.RWMutex
	metrics     *QueueMetrics
}

type QueueMetrics struct {
	queueLength prometheus.Gauge
	processingTime prometheus.Histogram
	messagesProcessed prometheus.Counter
	batchSize prometheus.Histogram
}

func NewQueue(numWorkers, batchSize int, batchWindow time.Duration) *Queue {
	// Create a unique registry for this queue instance
	reg := prometheus.NewRegistry()
	factory := promauto.With(reg)

	metrics := &QueueMetrics{
		queueLength: factory.NewGauge(prometheus.GaugeOpts{
			Name: "message_queue_length",
			Help: "Current number of messages in queue",
			ConstLabels: prometheus.Labels{"queue_id": fmt.Sprintf("queue_%d", time.Now().UnixNano())},
		}),
		processingTime: factory.NewHistogram(prometheus.HistogramOpts{
			Name:    "message_processing_time_seconds",
			Help:    "Time taken to process messages",
			Buckets: prometheus.DefBuckets,
			ConstLabels: prometheus.Labels{"queue_id": fmt.Sprintf("queue_%d", time.Now().UnixNano())},
		}),
		messagesProcessed: factory.NewCounter(prometheus.CounterOpts{
			Name: "messages_processed_total",
			Help: "Total number of processed messages",
			ConstLabels: prometheus.Labels{"queue_id": fmt.Sprintf("queue_%d", time.Now().UnixNano())},
		}),
		batchSize: factory.NewHistogram(prometheus.HistogramOpts{
			Name:    "message_batch_size",
			Help:    "Size of message batches",
			Buckets: []float64{1, 2, 5, 10, 20, 50},
			ConstLabels: prometheus.Labels{"queue_id": fmt.Sprintf("queue_%d", time.Now().UnixNano())},
		}),
	}

	q := &Queue{
		messages:    make(chan types.Message, 1000),
		workerPool:  NewWorkerPool(numWorkers),
		batchSize:   batchSize,
		batchWindow: batchWindow,
		batches:     make(map[types.MessageType]*MessageBatch),
		metrics:     metrics,
	}

	go q.batchProcessor()
	return q
}

func (q *Queue) Enqueue(msg types.Message) {
	q.messages <- msg
	q.metrics.queueLength.Inc()
}

func (q *Queue) batchProcessor() {
	ticker := time.NewTicker(q.batchWindow)
	defer ticker.Stop()

	for {
		select {
		case msg := <-q.messages:
			q.addToBatch(msg)
		case <-ticker.C:
			q.processBatches()
		}
	}
}

func (q *Queue) addToBatch(msg types.Message) {
	q.batchMutex.Lock()
	defer q.batchMutex.Unlock()

	if batch, exists := q.batches[msg.Type]; exists {
		batch.Messages = append(batch.Messages, msg)
		if len(batch.Messages) >= q.batchSize {
			q.processBatch(msg.Type)
		}
	} else {
		q.batches[msg.Type] = &MessageBatch{
			Type:     msg.Type,
			Messages: []types.Message{msg},
		}
	}
}

func (q *Queue) processBatches() {
	q.batchMutex.Lock()
	defer q.batchMutex.Unlock()

	for msgType := range q.batches {
		q.processBatch(msgType)
	}
}

func (q *Queue) processBatch(msgType types.MessageType) {
	if batch, exists := q.batches[msgType]; exists && len(batch.Messages) > 0 {
		start := time.Now()
		q.workerPool.Submit(func() {
			for range batch.Messages {
				q.metrics.queueLength.Dec()
				q.metrics.messagesProcessed.Inc()
			}
			q.metrics.batchSize.Observe(float64(len(batch.Messages)))
			q.metrics.processingTime.Observe(time.Since(start).Seconds())
		})
		delete(q.batches, msgType)
	}
}