package storage

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	iaa "github.com/intel-sandbox/go-iaa"
	"github.com/klauspost/compress/zstd"
	lz4 "github.com/pierrec/lz4/v4"
)

// compressor compresses individual frames. Implementations are pooled and
// reused across frames within a single CompressStream call.
type compressor interface {
	compress(src []byte) ([]byte, error)
	close() error
}

type compressorPool struct {
	pool sync.Pool
	mu   sync.Mutex
	all  []compressor
}

func (p *compressorPool) addCreated(c compressor) {
	p.mu.Lock()
	p.all = append(p.all, c)
	p.mu.Unlock()
}

func (p *compressorPool) Get() compressor {
	return p.pool.Get().(compressor)
}

func (p *compressorPool) Put(c compressor) {
	p.pool.Put(c)
}

func (p *compressorPool) Close() error {
	p.mu.Lock()
	all := p.all
	p.all = nil
	p.mu.Unlock()

	var closeErr error
	for _, c := range all {
		if err := c.close(); err != nil && closeErr == nil {
			closeErr = err
		}
	}

	return closeErr
}

type deflateCompressor struct {
	h *iaa.IAA_Handle
}

func (c *deflateCompressor) compress(src []byte) ([]byte, error) {
	dst := make([]byte, len(src))
	out, err := c.h.Compress(src, dst)
	if err != nil {
		return nil, fmt.Errorf("deflate compress: %w", err)
	}

	return out, nil
}

func (c *deflateCompressor) close() error {
	if c.h == nil {
		return nil
	}

	c.h.Close()
	c.h = nil
	return nil
}

// lz4Compressor wraps a pooled lz4.Writer. The writer is reused via Reset
// between frames to avoid re-allocating internal hash tables (~64KB).
type lz4Compressor struct {
	w *lz4.Writer
}

func (c *lz4Compressor) compress(src []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(lz4.CompressBlockBound(len(src)))
	c.w.Reset(&buf)

	if _, err := c.w.Write(src); err != nil {
		return nil, fmt.Errorf("lz4 compress: %w", err)
	}

	if err := c.w.Close(); err != nil {
		return nil, fmt.Errorf("lz4 compress close: %w", err)
	}

	return buf.Bytes(), nil
}

func (c *lz4Compressor) close() error { return nil }

// zstdCompressor wraps a pooled zstd.Encoder using EncodeAll.
type zstdCompressor struct {
	enc *zstd.Encoder
}

func (z *zstdCompressor) compress(src []byte) ([]byte, error) { //nolint:unparam // satisfies compressor interface
	return z.enc.EncodeAll(src, make([]byte, 0, len(src))), nil
}

func (z *zstdCompressor) close() error {
	if z.enc == nil {
		return nil
	}
	z.enc.Close()
	return nil
}

// newCompressorPool returns a pool of compressors for the given config.
// Both LZ4 and zstd encoders are pooled and reused via Reset/EncodeAll.
// The config is validated eagerly — if zstd options are invalid, an error
// is returned immediately rather than deferred to pool.Get().
func newCompressorPool(cfg CompressConfig) (*compressorPool, error) {
	pool := &compressorPool{}

	switch cfg.CompressionType() {
	case CompressionZstd:
		zstdOpts := []zstd.EOption{
			zstd.WithEncoderLevel(zstd.EncoderLevel(cfg.Level)),
			zstd.WithEncoderCRC(true),
		}
		if cfg.FrameSize() > 0 {
			zstdOpts = append(zstdOpts, zstd.WithWindowSize(cfg.FrameSize()))
		}
		if cfg.EncoderConcurrency > 0 {
			zstdOpts = append(zstdOpts, zstd.WithEncoderConcurrency(cfg.EncoderConcurrency))
		}

		// Validate options by creating one encoder upfront.
		first, err := zstd.NewWriter(nil, zstdOpts...)
		if err != nil {
			return nil, fmt.Errorf("zstd encoder: %w", err)
		}
		firstComp := &zstdCompressor{enc: first}
		pool.addCreated(firstComp)
		pool.Put(firstComp)

		pool.pool.New = func() any {
			// Options are already validated; NewWriter won't fail.
			enc, _ := zstd.NewWriter(nil, zstdOpts...)
			comp := &zstdCompressor{enc: enc}
			pool.addCreated(comp)
			return comp
		}
	case CompressionLZ4:
		lz4Opts := []lz4.Option{
			lz4.BlockSizeOption(lz4.Block4Mb),
			lz4.BlockChecksumOption(true),
			lz4.ChecksumOption(false),
			lz4.ConcurrencyOption(1),
			lz4.CompressionLevelOption(lz4.Fast),
		}

		// Validate options by creating one encoder upfront.
		first := lz4.NewWriter(nil)
		if err := first.Apply(lz4Opts...); err != nil {
			return nil, fmt.Errorf("lz4 encoder: %w", err)
		}
		firstComp := &lz4Compressor{w: first}
		pool.addCreated(firstComp)
		pool.Put(firstComp)

		pool.pool.New = func() any {
			w := lz4.NewWriter(nil)
			_ = w.Apply(lz4Opts...) //nolint:errcheck // options validated above
			comp := &lz4Compressor{w: w}
			pool.addCreated(comp)
			return comp
		}
	case CompressionDeflate:
		first, err := iaa.InitIAAHandle()
		if err != nil {
			return nil, fmt.Errorf("deflate encoder: %w", err)
		}
		firstComp := &deflateCompressor{h: first}
		pool.addCreated(firstComp)
		pool.Put(firstComp)

		pool.pool.New = func() any {
			h, err := iaa.InitIAAHandle() //nolint:errcheck // options validated above
			if err != nil {
				fmt.Errorf("deflate encoder: %w", err)
				return nil
			}
			comp := &deflateCompressor{h: h}
			pool.addCreated(comp)
			return comp
		}
	default:
		return nil, fmt.Errorf("unsupported compression type: %s", cfg.CompressionType())
	}

	return pool, nil
}

func CompressBytes(ctx context.Context, data []byte, cfg CompressConfig) (*FullFrameTable, []byte, [32]byte, error) {
	up := &memPartUploader{}

	const compressBytesConcurrency = 1
	ft, checksum, err := compressStream(ctx, bytes.NewReader(data), cfg, up, compressBytesConcurrency, nil)
	if err != nil {
		return nil, nil, [32]byte{}, err
	}

	return ft, up.Assemble(), checksum, nil
}
