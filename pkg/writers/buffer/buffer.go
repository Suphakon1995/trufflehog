package buffer

import (
	"bytes"
	"fmt"
	"sync"
	"time"

	"github.com/trufflesecurity/trufflehog/v3/pkg/context"
)

type poolMetrics struct{}

func (poolMetrics) recordShrink(amount int) {
	shrinkCount.Inc()
	shrinkAmount.Add(float64(amount))
}

func (poolMetrics) recordBufferRetrival() {
	activeBufferCount.Inc()
	checkoutCount.Inc()
	bufferCount.Inc()
}

func (poolMetrics) recordBufferReturn(bufCap, bufLen int64) {
	activeBufferCount.Dec()
	totalBufferSize.Add(float64(bufCap))
	totalBufferLength.Add(float64(bufLen))
}

// PoolOpts is a function that configures a BufferPool.
type PoolOpts func(pool *Pool)

// Pool of buffers.
type Pool struct {
	*sync.Pool
	bufferSize uint32

	metrics poolMetrics
}

const defaultBufferSize = 1 << 12 // 4KB
// NewBufferPool creates a new instance of BufferPool.
func NewBufferPool(opts ...PoolOpts) *Pool {
	pool := &Pool{bufferSize: defaultBufferSize}

	for _, opt := range opts {
		opt(pool)
	}
	pool.Pool = &sync.Pool{
		New: func() any {
			return NewRingBuffer(int(pool.bufferSize))
		},
	}

	return pool
}

// Get returns a Buffer from the pool.
func (p *Pool) Get(ctx context.Context) *Ring {
	buf, ok := p.Pool.Get().(*Ring)
	if !ok {
		ctx.Logger().Error(fmt.Errorf("Buffer pool returned unexpected type"), "using new Buffer")
		buf = NewRingBuffer(int(p.bufferSize))
	}
	p.metrics.recordBufferRetrival()
	// buf.resetMetric()

	return buf
}

// Put returns a Buffer to the pool.
func (p *Pool) Put(buf *Ring) {
	p.metrics.recordBufferReturn(int64(buf.Cap()), int64(buf.Len()))

	// If the Buffer is more than twice the default size, replace it with a new Buffer.
	// This prevents us from returning very large buffers to the pool.
	const maxAllowedCapacity = 2 * defaultBufferSize
	if buf.Cap() > maxAllowedCapacity {
		p.metrics.recordShrink(buf.Cap() - defaultBufferSize)
		buf = NewRingBuffer(int(p.bufferSize))
	}
	// buf.recordMetric()

	p.Pool.Put(buf)
}

// Buffer is a wrapper around bytes.Buffer that includes a timestamp for tracking Buffer checkout duration.
type Buffer struct {
	*bytes.Buffer
	checkedOutAt time.Time
}

// NewBuffer creates a new instance of Buffer.
func NewBuffer() *Buffer { return &Buffer{Buffer: bytes.NewBuffer(make([]byte, 0, defaultBufferSize))} }

func (r *Buffer) Grow(size int) {
	r.Buffer.Grow(size)
	r.recordGrowth(size)
}

func (r *Buffer) resetMetric() { r.checkedOutAt = time.Now() }

func (r *Buffer) recordMetric() {
	dur := time.Since(r.checkedOutAt)
	checkoutDuration.Observe(float64(dur.Microseconds()))
	checkoutDurationTotal.Add(float64(dur.Microseconds()))
}

func (r *Buffer) recordGrowth(size int) {
	growCount.Inc()
	growAmount.Add(float64(size))
}

// Write date to the buffer.
func (r *Buffer) Write(ctx context.Context, data []byte) (int, error) {
	if r.Buffer == nil {
		// This case should ideally never occur if buffers are properly managed.
		ctx.Logger().Error(fmt.Errorf("buffer is nil, initializing a new buffer"), "action", "initializing_new_buffer")
		r.Buffer = bytes.NewBuffer(make([]byte, 0, defaultBufferSize))
		r.resetMetric()
	}

	size := len(data)
	bufferLength := r.Buffer.Len()
	totalSizeNeeded := bufferLength + size
	// If the total size is within the threshold, write to the buffer.
	ctx.Logger().V(4).Info(
		"writing to buffer",
		"data_size", size,
		"content_size", bufferLength,
	)

	availableSpace := r.Buffer.Cap() - bufferLength
	growSize := totalSizeNeeded - bufferLength
	if growSize > availableSpace {
		ctx.Logger().V(4).Info(
			"buffer size exceeded, growing buffer",
			"current_size", bufferLength,
			"new_size", totalSizeNeeded,
			"available_space", availableSpace,
			"grow_size", growSize,
		)
		// We are manually growing the buffer so we can track the growth via metrics.
		// Knowing the exact data size, we directly resize to fit it, rather than exponential growth
		// which may require multiple allocations and copies if the size required is much larger
		// than double the capacity. Our approach aligns with default behavior when growth sizes
		// happen to match current capacity, retaining asymptotic efficiency benefits.
		r.Buffer.Grow(growSize)
	}

	return r.Buffer.Write(data)
}

// readCloser is a custom implementation of io.ReadCloser. It wraps a bytes.Reader
// for reading data from an in-memory buffer and includes an onClose callback.
// The onClose callback is used to return the buffer to the pool, ensuring buffer re-usability.
type readCloser struct {
	*bytes.Reader
	onClose func()
}

// ReadCloser creates a new instance of readCloser.
func ReadCloser(data []byte, onClose func()) *readCloser {
	return &readCloser{Reader: bytes.NewReader(data), onClose: onClose}
}

// Close implements the io.Closer interface. It calls the onClose callback to return the buffer
// to the pool, enabling buffer reuse. This method should be called by the consumers of ReadCloser
// once they have finished reading the data to ensure proper resource management.
func (brc *readCloser) Close() error {
	if brc.onClose == nil {
		return nil
	}

	brc.onClose() // Return the buffer to the pool
	return nil
}
