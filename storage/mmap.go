//go:build !wasm
// +build !wasm

package storage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/anacrolix/missinggo/v2"
	"github.com/edsrzf/mmap-go"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/mmap_span"
	"github.com/anacrolix/torrent/types/infohash"
)

type mmapClientImpl struct {
	baseDir string
	pc      PieceCompletion
}

// TODO: Support all the same native filepath configuration that NewFileOpts provides.
func NewMMap(baseDir string) ClientImplCloser {
	return NewMMapWithCompletion(baseDir, pieceCompletionForDir(baseDir))
}

func NewMMapWithCompletion(baseDir string, completion PieceCompletion) *mmapClientImpl {
	return &mmapClientImpl{
		baseDir: baseDir,
		pc:      completion,
	}
}

func (s *mmapClientImpl) OpenTorrent(info *metainfo.Info, infoHash metainfo.Hash) (_ TorrentImpl, err error) {
	span, err := mMapTorrent(info, infoHash, s.baseDir, 15*time.Second, func(infoHash infohash.T, flushed *roaring.Bitmap) {
		if committer, ok := s.pc.(interface {
			Flushed(infoHash infohash.T, flushed *roaring.Bitmap)
		}); ok {
			committer.Flushed(infoHash, flushed)
		}
	})
	t := &mmapTorrentStorage{
		infoHash: infoHash,
		span:     span,
		pc:       s.pc,
	}
	return TorrentImpl{Piece: t.Piece, Close: t.Close, Flush: t.Flush}, err
}

func (s *mmapClientImpl) Close() error {
	return s.pc.Close()
}

type mmapTorrentStorage struct {
	infoHash metainfo.Hash
	span     *mmap_span.MMapSpan
	pc       PieceCompletionGetSetter
}

type writerAtFunc func(p []byte, off int64) (n int, err error)

func (f writerAtFunc) WriteAt(p []byte, off int64) (n int, err error) {
	return f(p, off)
}

func (ts *mmapTorrentStorage) Piece(p metainfo.Piece) PieceImpl {
	return mmapStoragePiece{
		pc:       ts.pc,
		p:        p,
		ih:       ts.infoHash,
		ReaderAt: io.NewSectionReader(ts.span, p.Offset(), p.Length()),
		WriterAt: missinggo.NewSectionWriter(
			writerAtFunc(func(b []byte, off int64) (n int, err error) {
				return ts.span.WriteAt(p.Index(), b, off)
			}), p.Offset(), p.Length()),
	}
}

func (ts *mmapTorrentStorage) Close() error {
	errs := ts.span.Close()
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

func (ts *mmapTorrentStorage) Flush(onFlush func(size int64)) error {
	errs := ts.span.Flush(onFlush)
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

type mmapStoragePiece struct {
	pc      PieceCompletionGetSetter
	p       metainfo.Piece
	ih      metainfo.Hash
	created bool
	io.ReaderAt
	io.WriterAt
}

func (me mmapStoragePiece) pieceKey() metainfo.PieceKey {
	return metainfo.PieceKey{me.ih, me.p.Index()}
}

func (sp mmapStoragePiece) Completion() Completion {
	c, err := sp.pc.Get(sp.pieceKey())
	if err != nil {
		panic(err)
	}
	return c
}

func (sp mmapStoragePiece) MarkComplete(awaitFlush bool) error {
	sp.pc.Set(sp.pieceKey(), true, awaitFlush)
	return nil
}

func (sp mmapStoragePiece) MarkNotComplete() error {
	sp.pc.Set(sp.pieceKey(), false, false)
	return nil
}

func (sp mmapStoragePiece) IsNew() bool {
	return sp.created
}

func mMapTorrent(md *metainfo.Info, infoHash metainfo.Hash, location string, flushTime time.Duration, flushedCallback mmap_span.FlushedCallback) (mms *mmap_span.MMapSpan, err error) {
	mms = &mmap_span.MMapSpan{
		InfoHash:        infoHash,
		FlushTime:       flushTime,
		FlushedCallback: flushedCallback,
	}

	defer func() {
		if err != nil {
			mms.Close()
		}
	}()

	for _, miFile := range md.UpvertedFiles() {
		var safeName string
		safeName, err = ToSafeFilePath(append([]string{md.Name}, miFile.Path...)...)
		if err != nil {
			return
		}
		fileName := filepath.Join(location, safeName)
		var mm FileMapping
		var exists bool
		mm, exists, err = mmapFile(fileName, miFile.Length)
		if err != nil {
			err = fmt.Errorf("file %q: %s", miFile.DisplayPath(md), err)
			return
		}
		mms.Append(mm)
		if !exists {
			mms.Created = true
		}
	}
	mms.InitIndex()
	return
}

func mmapFile(name string, size int64) (_ FileMapping, exists bool, err error) {
	dir := filepath.Dir(name)
	err = os.MkdirAll(dir, 0o750)
	if err != nil {
		err = fmt.Errorf("making directory %q: %s", dir, err)
		return
	}

	_, err = os.Stat(name)

	exists = err == nil || !errors.Is(err, os.ErrNotExist)

	var file *os.File
	file, err = os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			file.Close()
		}
	}()
	var fi os.FileInfo
	fi, err = file.Stat()
	if err != nil {
		return
	}
	if fi.Size() < size {
		// I think this is necessary on HFS+. Maybe Linux will SIGBUS too if
		// you overmap a file but I'm not sure.
		err = file.Truncate(size)
		if err != nil {
			return
		}
		// if the file needs to be resized assume we're creating it
		exists = false
	}
	return func() (ret mmapWithFile, ex bool, err error) {
		ret.f = file
		ex = exists

		if size == 0 {
			// Can't mmap() regions with length 0.
			return
		}
		intLen := int(size)
		if int64(intLen) != size {
			err = errors.New("size too large for system")
			return
		}
		ret.mmap, err = mmap.MapRegion(file, intLen, mmap.RDWR, 0, 0)
		if err != nil {
			err = fmt.Errorf("error mapping region: %s", err)
			return
		}
		if int64(len(ret.mmap)) != size {
			panic(len(ret.mmap))
		}
		return
	}()
}

// Combines a mmapped region and file into a storage Mmap abstraction, which handles closing the
// mmap file handle.
func WrapFileMapping(region mmap.MMap, file *os.File) FileMapping {
	return mmapWithFile{
		f:    file,
		mmap: region,
	}
}

type FileMapping = mmap_span.Mmap

// Handles closing the mmap's file handle (needed for Windows). Could be implemented differently by
// OS.
type mmapWithFile struct {
	f       *os.File
	created bool
	mmap    mmap.MMap
}

func (m mmapWithFile) Flush() error {
	return m.mmap.Flush()
}

func (m mmapWithFile) IsLocal() bool {
	return !m.created
}

func (m mmapWithFile) Unmap() (err error) {
	if m.mmap != nil {
		err = m.mmap.Unmap()
	}
	fileErr := m.f.Close()
	if err == nil {
		err = fileErr
	}
	return
}

func (m mmapWithFile) Bytes() []byte {
	if m.mmap == nil {
		return nil
	}
	return m.mmap
}
