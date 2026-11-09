package mordesync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
)

// ReadBlocks reads only the requested blocks and may call consume concurrently
// from up to four workers. At most four 4 MiB buffers are in use across this
// module. Data is borrowed until consume returns; callers must copy it before
// retaining it asynchronously and synchronize any shared callback state.
func ReadBlocks(ctx context.Context, directory string, blocks []BlockRef, consume BlockConsumer) error {
	if ctx == nil || consume == nil {
		return errors.New("context and block consumer are required")
	}
	if err := acquireOperation(ctx); err != nil {
		return err
	}
	defer func() { <-operationSlots }()
	for _, ref := range blocks {
		if err := validateFile(ref.File); err != nil || ref.Index < 0 || ref.Index >= len(ref.File.Blocks) {
			return ErrInvalidManifest
		}
	}
	if len(blocks) == 0 {
		return nil
	}
	root, err := openDirectoryRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	workerCount := min(Parallelism, len(blocks))
	var workers sync.WaitGroup
	workers.Add(workerCount)
	var firstErr error
	var once sync.Once
	setErr := func(err error) {
		if err == nil {
			return
		}
		once.Do(func() {
			firstErr = err
			cancel()
		})
	}
	for range workerCount {
		go func() {
			defer workers.Done()
			for index := range jobs {
				if workerCtx.Err() != nil {
					return
				}
				ref := blocks[index]
				block := ref.File.Blocks[ref.Index]
				err := withBlockBuffer(workerCtx, func(buffer []byte) error {
					data, err := readBlock(workerCtx, root, ref.File, block, buffer)
					if err != nil {
						return err
					}
					return consume(ref.File, ref.Index, block, data)
				})
				if err != nil {
					setErr(err)
					return
				}
			}
		}()
	}
sendJobs:
	for index := range blocks {
		select {
		case jobs <- index:
		case <-workerCtx.Done():
			break sendJobs
		}
	}
	close(jobs)
	workers.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func readBlock(ctx context.Context, root *os.Root, file File, block Block, buffer []byte) ([]byte, error) {
	opened, err := openFile(root, file.Path, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("open source %q: %w", file.Path, ErrRescanRequired)
	}
	defer opened.Close()
	before, err := opened.Stat()
	if err != nil || !matchesManifest(before, file) {
		return nil, fmt.Errorf("source %q changed: %w", file.Path, ErrRescanRequired)
	}
	data := buffer[:int(block.Size)]
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n, err := opened.ReadAt(data, block.Offset)
	if err != nil || n != len(data) {
		return nil, fmt.Errorf("read source block %q: %w", file.Path, ErrRescanRequired)
	}
	if sumHex(data) != block.SHA256 {
		return nil, fmt.Errorf("source block %q changed: %w", file.Path, ErrRescanRequired)
	}
	after, err := opened.Stat()
	current, pathErr := lstatPath(root, file.Path)
	if err != nil || pathErr != nil || !os.SameFile(before, after) || !os.SameFile(before, current) ||
		!matchesManifest(after, file) || !matchesManifest(current, file) {
		return nil, fmt.Errorf("source %q changed while reading: %w", file.Path, ErrRescanRequired)
	}
	return data, nil
}

func matchesManifest(info os.FileInfo, file File) bool {
	return info != nil && info.Mode().IsRegular() && info.Size() == file.Size &&
		uint32(info.Mode().Perm()) == file.Mode && (!file.ModTimeKnown || info.ModTime().UnixNano() == file.ModTimeNS)
}
