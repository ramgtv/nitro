package plasma

import (
	"fmt"
	"github.com/couchbase/nitro/mm"
	"github.com/couchbase/nitro/skiplist"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

type PageReader func(offset LSSOffset) (Page, error)

const maxCtxBuffers = 8
const (
	bufEncPage int = iota
	bufEncMeta
	bufTempItem
	bufReloc
	bufCleaner
	bufRecovery
	bufFetch
	bufPersist
)

const recoverySMRInterval = 100

var (
	memQuota       int64
	maxMemoryQuota = int64(1024 * 1024 * 1024 * 1024)
	dbInstances    *skiplist.Skiplist
)

func init() {
	dbInstances = skiplist.New()
	SetMemoryQuota(maxMemoryQuota)
}

type Plasma struct {
	Config
	*skiplist.Skiplist
	wlist                           []*Writer
	lss                             LSS
	lssCleanerWriter                *wCtx
	persistWriters                  []*wCtx
	evictWriters                    []*wCtx
	stoplssgc, stopswapper, stopmon chan struct{}
	sync.RWMutex

	// MVCC data structures
	itemsCount   int64
	mvcc         sync.RWMutex
	currSn       uint64
	numSnCreated int
	gcSn         uint64
	currSnapshot *Snapshot

	lastMaxSn uint64

	rpSns          unsafe.Pointer
	rpVersion      uint16
	recoveryPoints []*RecoveryPoint

	hasMemoryPressure bool
	clockHandle       *clockHandle
	clockLock         sync.Mutex

	smrWg   sync.WaitGroup
	smrChan chan unsafe.Pointer

	*storeCtx

	wCtxLock sync.Mutex
	wCtxList *wCtx
	gCtx     *wCtx
}

type Stats struct {
	Compacts int64
	Splits   int64
	Merges   int64
	Inserts  int64
	Deletes  int64

	CompactConflicts int64
	SplitConflicts   int64
	MergeConflicts   int64
	InsertConflicts  int64
	DeleteConflicts  int64
	SwapInConflicts  int64

	BytesIncoming int64
	BytesWritten  int64

	FlushDataSz int64

	MemSz      int64
	MemSzIndex int64

	AllocSz   int64
	FreeSz    int64
	ReclaimSz int64

	NumRecordAllocs  int64
	NumRecordFrees   int64
	NumRecordSwapOut int64
	NumRecordSwapIn  int64
	AllocSzIndex     int64
	FreeSzIndex      int64
	ReclaimSzIndex   int64

	NumPages int64

	LSSFrag      int
	LSSDataSize  int64
	LSSUsedSpace int64
	NumLSSReads  int64
	LSSReadBytes int64

	NumLSSCleanerReads  int64
	LSSCleanerReadBytes int64

	CacheHits   int64
	CacheMisses int64

	WriteAmp      float64
	WriteAmpAvg   float64
	CacheHitRatio float64
	ResidentRatio float64
}

func (s *Stats) Merge(o *Stats) {
	s.Compacts += o.Compacts
	s.Splits += o.Splits
	s.Merges += o.Merges
	s.Inserts += o.Inserts
	s.Deletes += o.Deletes

	s.CompactConflicts += o.CompactConflicts
	s.SplitConflicts += o.SplitConflicts
	s.MergeConflicts += o.MergeConflicts
	s.InsertConflicts += o.InsertConflicts
	s.DeleteConflicts += o.DeleteConflicts
	s.SwapInConflicts += o.SwapInConflicts

	s.AllocSz += o.AllocSz
	s.FreeSz += o.FreeSz
	s.ReclaimSz += o.ReclaimSz

	s.AllocSzIndex += o.AllocSzIndex
	s.FreeSzIndex += o.FreeSzIndex
	o.ReclaimSzIndex += o.ReclaimSzIndex

	s.NumRecordAllocs += o.NumRecordAllocs
	s.NumRecordFrees += o.NumRecordFrees
	s.NumRecordSwapOut += o.NumRecordSwapOut
	s.NumRecordSwapIn += o.NumRecordSwapIn

	s.BytesIncoming += o.BytesIncoming

	s.NumLSSReads += o.NumLSSReads
	s.LSSReadBytes += o.LSSReadBytes

	s.CacheHits += o.CacheHits
	s.CacheMisses += o.CacheMisses
}

func (s Stats) String() string {
	return fmt.Sprintf("===== Stats =====\n"+
		"memory_quota      = %d\n"+
		"count             = %d\n"+
		"compacts          = %d\n"+
		"splits            = %d\n"+
		"merges            = %d\n"+
		"inserts           = %d\n"+
		"deletes           = %d\n"+
		"compact_conflicts = %d\n"+
		"split_conflicts   = %d\n"+
		"merge_conflicts   = %d\n"+
		"insert_conflicts  = %d\n"+
		"delete_conflicts  = %d\n"+
		"swapin_conflicts  = %d\n"+
		"memory_size       = %d\n"+
		"memory_size_index = %d\n"+
		"allocated         = %d\n"+
		"freed             = %d\n"+
		"reclaimed         = %d\n"+
		"reclaim_pending   = %d\n"+
		"allocated_index   = %d\n"+
		"freed_index       = %d\n"+
		"reclaimed_index   = %d\n"+
		"num_pages         = %d\n"+
		"num_rec_allocs    = %d\n"+
		"num_rec_frees     = %d\n"+
		"num_rec_swapout   = %d\n"+
		"num_rec_swapin    = %d\n"+
		"bytes_incoming    = %d\n"+
		"bytes_written     = %d\n"+
		"write_amp         = %.2f\n"+
		"write_amp_avg     = %.2f\n"+
		"lss_fragmentation = %d%%\n"+
		"lss_data_size     = %d\n"+
		"lss_used_space    = %d\n"+
		"lss_num_reads     = %d\n"+
		"lss_read_bs       = %d\n"+
		"lss_gc_num_reads  = %d\n"+
		"lss_gc_reads_bs   = %d\n"+
		"cache_hits        = %d\n"+
		"cache_misses      = %d\n"+
		"cache_hit_ratio   = %.2f\n"+
		"resident_ratio    = %.2f\n",
		atomic.LoadInt64(&memQuota),
		s.Inserts-s.Deletes,
		s.Compacts, s.Splits, s.Merges,
		s.Inserts, s.Deletes, s.CompactConflicts,
		s.SplitConflicts, s.MergeConflicts,
		s.InsertConflicts, s.DeleteConflicts,
		s.SwapInConflicts, s.MemSz, s.MemSzIndex,
		s.AllocSz, s.FreeSz, s.ReclaimSz,
		s.FreeSz-s.ReclaimSz,
		s.AllocSzIndex, s.FreeSzIndex, s.ReclaimSzIndex,
		s.NumPages, s.NumRecordAllocs, s.NumRecordFrees,
		s.NumRecordSwapOut, s.NumRecordSwapIn,
		s.BytesIncoming, s.BytesWritten,
		s.WriteAmp, s.WriteAmpAvg,
		s.LSSFrag, s.LSSDataSize, s.LSSUsedSpace,
		s.NumLSSReads, s.LSSReadBytes,
		s.NumLSSCleanerReads, s.LSSCleanerReadBytes,
		s.CacheHits, s.CacheMisses, s.CacheHitRatio,
		s.ResidentRatio)
}

func New(cfg Config) (*Plasma, error) {
	var err error

	cfg = applyConfigDefaults(cfg)

	s := &Plasma{Config: cfg}
	slCfg := skiplist.DefaultConfig()
	if cfg.UseMemoryMgmt {
		s.smrChan = make(chan unsafe.Pointer, smrChanBufSize)
		slCfg.UseMemoryMgmt = true
		slCfg.Malloc = mm.Malloc
		slCfg.Free = mm.Free
		slCfg.BarrierDestructor = s.newBSDestroyCallback()
	}

	sl := skiplist.NewWithConfig(slCfg)
	s.Skiplist = sl

	var cfGetter, lfGetter FilterGetter
	if cfg.EnableShapshots {
		cfGetter = func() ItemFilter {
			gcSn := atomic.LoadUint64(&s.gcSn) + 1
			rpSns := (*[]uint64)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&s.rpSns))))

			var gcPos int
			for _, sn := range *rpSns {
				if sn < gcSn {
					gcPos++
				} else {
					break
				}
			}

			var snIntervals []uint64
			if gcPos == 0 {
				snIntervals = []uint64{0, gcSn}
			} else {
				snIntervals = make([]uint64, gcPos+2)
				copy(snIntervals[1:], (*rpSns)[:gcPos])
				snIntervals[gcPos+1] = gcSn
			}

			return &gcFilter{snIntervals: snIntervals}
		}

		lfGetter = func() ItemFilter {
			return &rollbackFilter{}
		}
	} else {
		cfGetter = func() ItemFilter {
			return new(defaultFilter)
		}

		lfGetter = func() ItemFilter {
			return &nilFilter
		}
	}

	s.storeCtx = newStoreContext(sl, cfg.UseMemoryMgmt, cfg.ItemSize,
		cfg.Compare, cfGetter, lfGetter)

	s.gCtx = s.newWCtx()
	if s.useMemMgmt {
		s.smrWg.Add(1)
		go s.smrWorker(s.gCtx)
	}

	sbuf := dbInstances.MakeBuf()
	defer dbInstances.FreeBuf(sbuf)
	dbInstances.Insert(unsafe.Pointer(s), ComparePlasma, sbuf, &dbInstances.Stats)

	if s.shouldPersist {
		commitDur := time.Duration(cfg.SyncInterval) * time.Second
		s.lss, err = NewLSStore(cfg.File, cfg.LSSLogSegmentSize, cfg.FlushBufferSize, 2, cfg.UseMmap, commitDur)
		if err != nil {
			return nil, err
		}

		s.lss.SetSafeTrimCallback(s.findSafeLSSTrimOffset)
		s.initLRUClock()
		err = s.doRecovery()
	}

	s.doInit()

	if s.shouldPersist {
		s.persistWriters = make([]*wCtx, runtime.NumCPU())
		s.evictWriters = make([]*wCtx, runtime.NumCPU())
		for i, _ := range s.persistWriters {
			s.persistWriters[i] = s.newWCtx()
			s.evictWriters[i] = s.newWCtx()
		}
		s.lssCleanerWriter = s.newWCtx()

		s.stoplssgc = make(chan struct{})
		s.stopswapper = make(chan struct{})
		s.stopmon = make(chan struct{})

		if cfg.AutoLSSCleaning {
			go s.lssCleanerDaemon()
		}

		if cfg.AutoSwapper {
			go s.swapperDaemon()
		}
	}

	go s.monitorMemUsage()
	go s.runtimeStats()
	return s, err
}

func (s *Plasma) runtimeStats() {
	so := s.GetStats()
	for {
		select {
		case <-s.stopmon:
			return
		default:
		}

		time.Sleep(time.Second * 5)

		now := s.GetStats()
		bsOut := (float64(now.BytesWritten) - float64(so.BytesWritten))
		bsIn := (float64(now.BytesIncoming) - float64(so.BytesIncoming))
		if bsIn > 0 {
			s.gCtx.sts.WriteAmp = bsOut / bsIn
		}

		hits := now.CacheHits - so.CacheHits
		miss := now.CacheMisses - so.CacheMisses
		if tot := float64(hits + miss); tot > 0 {
			s.gCtx.sts.CacheHitRatio = float64(hits) / tot
		}
		so = now
	}
}

func (s *Plasma) monitorMemUsage() {
	sctx := s.newWCtx2().SwapperContext()

	for {
		select {
		case <-s.stopmon:
			return
		default:
		}
		s.hasMemoryPressure = s.TriggerSwapper(sctx)
		time.Sleep(time.Millisecond * 100)
	}
}

func (s *Plasma) doInit() {
	// Init seed page if page-0 does not exist even after recovery
	pid := s.StartPageId()
	if pid.(*skiplist.Node).Link == nil {
		pg := s.newSeedPage(s.gCtx)
		s.CreateMapping(pid, pg, s.gCtx)
	}

	if s.EnableShapshots {
		if s.currSn == 0 {
			s.currSn = 1
		}

		s.currSnapshot = &Snapshot{
			sn:       s.currSn,
			refCount: 1,
			db:       s,
		}

		s.updateMaxSn(s.currSn, true)
		s.updateRecoveryPoints(s.recoveryPoints)
		s.updateRPSns(s.recoveryPoints)
	}
}

func (s *Plasma) doRecovery() error {
	pg := newPage(s.gCtx, nil, nil).(*page)

	buf := s.gCtx.GetBuffer(bufRecovery)

	fn := func(offset LSSOffset, bs []byte) (bool, error) {
		typ := getLSSBlockType(bs)
		bs = bs[lssBlockTypeSize:]
		switch typ {
		case lssDiscard:
		case lssRecoveryPoints:
			s.rpVersion, s.recoveryPoints = unmarshalRPs(bs)
		case lssMaxSn:
			s.currSn = decodeMaxSn(bs)
		case lssPageRemove:
			rmPglow := getRmPageLow(bs)
			pid := s.getPageId(rmPglow, s.gCtx)
			if pid != nil {
				currPg, err := s.ReadPage(pid, s.gCtx.pgRdrFn, false, s.gCtx)
				if err != nil {
					return false, err
				}

				// TODO: Store precomputed fdSize in swapout delta
				s.gCtx.sts.FlushDataSz -= int64(currPg.GetFlushDataSize())
				currPg.(*page).free(false)
				s.unindexPage(pid, s.gCtx)
			}
		case lssPageData, lssPageReloc, lssPageUpdate:
			pg.Unmarshal(bs, s.gCtx)
			flushDataSz := len(bs)

			newPageData := (typ == lssPageData || typ == lssPageReloc)
			if pid := s.getPageId(pg.low, s.gCtx); pid == nil {
				if newPageData {
					s.gCtx.sts.FlushDataSz += int64(flushDataSz)
					pg.AddFlushRecord(offset, flushDataSz, 1)
					pid = s.AllocPageId(s.gCtx)
					s.CreateMapping(pid, pg, s.gCtx)
					s.indexPage(pid, s.gCtx)
				} else {
					pg.free(false)
				}
			} else {
				s.gCtx.sts.FlushDataSz += int64(flushDataSz)

				currPg, err := s.ReadPage(pid, s.gCtx.pgRdrFn, false, s.gCtx)
				if err != nil {
					return false, err
				}

				if newPageData {
					s.gCtx.sts.FlushDataSz -= int64(currPg.GetFlushDataSize())
					currPg.(*page).free(false)
					pg.AddFlushRecord(offset, flushDataSz, 1)
				} else {
					_, numSegments, _ := currPg.GetFlushInfo()
					pg.Append(currPg)
					pg.AddFlushRecord(offset, flushDataSz, numSegments+1)
				}

				pg.prevHeadPtr = currPg.(*page).prevHeadPtr
				s.UpdateMapping(pid, pg, s.gCtx)
			}
		}

		pg.Reset()
		s.tryEvictPages(s.gCtx)
		s.trySMRObjects(s.gCtx, recoverySMRInterval)
		return true, nil
	}

	err := s.lss.Visitor(fn, buf)
	if err != nil {
		return err
	}

	s.trySMRObjects(s.gCtx, 0)

	// Initialize rightSiblings for all pages
	var lastPg Page
	callb := func(pid PageId, partn RangePartition) error {
		pg, err := s.ReadPage(pid, s.gCtx.pgRdrFn, false, s.gCtx)
		if lastPg != nil {
			if err == nil && s.cmp(lastPg.MaxItem(), pg.MinItem()) != 0 {
				panic("found missing page")
			}

			lastPg.SetNext(pid)
		}

		lastPg = pg
		return err
	}

	s.PageVisitor(callb, 1)
	s.gcSn = s.currSn

	if lastPg != nil {
		lastPg.SetNext(s.EndPageId())
		if lastPg.MaxItem() != skiplist.MaxItem {
			panic("invalid last page")
		}
	}

	return err
}

func (s *Plasma) Close() {
	if s.EnableShapshots {
		// Force SMR flush
		s.NewSnapshot().Close()
	}
	close(s.stopmon)
	if s.Config.AutoLSSCleaning {
		s.stoplssgc <- struct{}{}
		<-s.stoplssgc
	}

	if s.Config.AutoSwapper {
		s.stopswapper <- struct{}{}
		<-s.stopswapper
	}

	if s.Config.shouldPersist {
		s.lss.Close()
	}

	sbuf := dbInstances.MakeBuf()
	defer dbInstances.FreeBuf(sbuf)
	dbInstances.Delete(unsafe.Pointer(s), ComparePlasma, sbuf, &dbInstances.Stats)

	if s.useMemMgmt {
		close(s.smrChan)
		s.smrWg.Wait()
		s.destroyAllObjects()
	}
}

func ComparePlasma(a, b unsafe.Pointer) int {
	return int(uintptr(a)) - int(uintptr(b))
}

type Writer struct {
	*wCtx
	count int64
}

type Reader struct {
	iter *MVCCIterator
}

// TODO: Refactor wCtx and Writer
type wCtx struct {
	*Plasma
	buf       *skiplist.ActionBuffer
	pgBuffers [][]byte
	slSts     *skiplist.Stats
	sts       *Stats
	dbIter    *skiplist.Iterator

	pgRdrFn PageReader

	pgAllocCtx *allocCtx

	reclaimList []reclaimObject

	next *wCtx

	safeOffset LSSOffset
}

func (ctx *wCtx) freePages(pages []pgFreeObj) {
	for _, pg := range pages {
		nr, size := computeMemUsed(pg.h, ctx.itemSize)
		ctx.sts.FreeSz += int64(size)

		ctx.sts.NumRecordFrees += int64(nr)
		if pg.evicted {
			ctx.sts.NumRecordSwapOut += int64(nr)
		}

		if ctx.useMemMgmt {
			o := reclaimObject{typ: smrPage, size: uint32(size),
				ptr: unsafe.Pointer(pg.h)}
			ctx.reclaimList = append(ctx.reclaimList, o)
		}
	}
}

func (ctx *wCtx) SwapperContext() SwapperContext {
	return ctx.dbIter
}

func (s *Plasma) newWCtx() *wCtx {
	s.wCtxLock.Lock()
	defer s.wCtxLock.Unlock()

	ctx := s.newWCtx2()
	s.wCtxList = ctx
	return ctx
}

func (s *Plasma) newWCtx2() *wCtx {
	ctx := &wCtx{
		Plasma:     s,
		pgAllocCtx: new(allocCtx),
		buf:        s.Skiplist.MakeBuf(),
		slSts:      &s.Skiplist.Stats,
		sts:        new(Stats),
		pgBuffers:  make([][]byte, maxCtxBuffers),
		next:       s.wCtxList,
		safeOffset: expiredLSSOffset,
	}

	ctx.dbIter = dbInstances.NewIterator(ComparePlasma, ctx.buf)
	ctx.pgRdrFn = func(offset LSSOffset) (Page, error) {
		return s.fetchPageFromLSS(offset, ctx)
	}

	return ctx
}

func (ctx *wCtx) GetBuffer(id int) []byte {
	if ctx.pgBuffers[id] == nil {
		ctx.pgBuffers[id] = make([]byte, maxPageEncodedSize)
	}

	return ctx.pgBuffers[id]
}

func (s *Plasma) NewWriter() *Writer {

	w := &Writer{
		wCtx: s.newWCtx(),
	}

	s.Lock()
	defer s.Unlock()

	s.wlist = append(s.wlist, w)
	if s.useMemMgmt {
		s.smrWg.Add(1)
		go s.smrWorker(w.wCtx)
	}

	return w
}

func (s *Plasma) NewReader() *Reader {
	iter := s.NewIterator().(*Iterator)
	iter.filter = &snFilter{}

	return &Reader{
		iter: &MVCCIterator{
			Iterator: iter,
		},
	}
}

func (r *Reader) NewSnapshotIterator(snap *Snapshot) *MVCCIterator {
	snap.Open()
	r.iter.filter.(*snFilter).sn = snap.sn
	r.iter.token = r.iter.BeginTx()
	r.iter.snap = snap
	return r.iter
}

func (s *Plasma) MemoryInUse() int64 {
	var memSz int64
	for w := s.wCtxList; w != nil; w = w.next {
		memSz += w.sts.AllocSz - w.sts.FreeSz
		memSz += w.sts.AllocSzIndex - w.sts.FreeSzIndex
	}

	return memSz
}

func (s *Plasma) GetStats() Stats {
	var sts Stats

	sts.NumPages = int64(s.Skiplist.GetStats().NodeCount + 1)
	for w := s.wCtxList; w != nil; w = w.next {
		sts.Merge(w.sts)
	}

	sts.MemSz = sts.AllocSz - sts.FreeSz
	sts.MemSzIndex = sts.AllocSzIndex - sts.FreeSzIndex
	if s.shouldPersist {
		sts.BytesWritten = s.lss.BytesWritten()
		sts.LSSFrag, sts.LSSDataSize, sts.LSSUsedSpace = s.GetLSSInfo()
		sts.NumLSSCleanerReads = s.lssCleanerWriter.sts.NumLSSReads
		sts.LSSCleanerReadBytes = s.lssCleanerWriter.sts.LSSReadBytes
		sts.CacheHitRatio = s.gCtx.sts.CacheHitRatio
		sts.WriteAmp = s.gCtx.sts.WriteAmp
		bsOut := float64(sts.BytesWritten)
		bsIn := float64(sts.BytesIncoming)
		if bsIn > 0 {
			sts.WriteAmpAvg = bsOut / bsIn
		}
		cachedRecs := sts.NumRecordAllocs - sts.NumRecordFrees
		lssRecs := sts.NumRecordSwapOut - sts.NumRecordSwapIn
		totalRecs := cachedRecs + lssRecs
		if totalRecs > 0 {
			sts.ResidentRatio = float64(cachedRecs) / float64(totalRecs)
		}
	}
	return sts
}

func (s *Plasma) LSSDataSize() int64 {
	var sz int64

	for w := s.wCtxList; w != nil; w = w.next {
		sz += w.sts.FlushDataSz
	}

	return sz
}

func (s *Plasma) indexPage(pid PageId, ctx *wCtx) {
	n := pid.(*skiplist.Node)
	if n.Item() == skiplist.MinItem {
		link := n.Link
		s.FreePageId(pid, ctx)
		n = s.StartPageId().(*skiplist.Node)
		n.Link = link
		return
	}
retry:
	if existNode, ok := s.Skiplist.Insert4(n, s.cmp, s.cmp, ctx.buf, n.Level(), false, false, ctx.slSts); !ok {
		if pg := newPage(ctx, nil, existNode.Link); pg.NeedRemoval() {
			runtime.Gosched()
			goto retry
		}
		panic("duplicate index node")
	}

	ctx.sts.AllocSzIndex += int64(s.itemSize(n.Item()) + uintptr(n.Size()))
}

func (s *Plasma) unindexPage(pid PageId, ctx *wCtx) {
	n := pid.(*skiplist.Node)
	s.Skiplist.DeleteNode2(n, s.cmp, ctx.buf, ctx.slSts)
	size := int64(s.itemSize(n.Item()) + uintptr(n.Size()))
	ctx.sts.FreeSzIndex += size

	if s.useMemMgmt {
		o := reclaimObject{typ: smrPageId, size: uint32(size), ptr: unsafe.Pointer(n)}
		ctx.reclaimList = append(ctx.reclaimList, o)
	}
}

func (s *Plasma) tryPageRemoval(pid PageId, pg Page, ctx *wCtx) {
	itm := pg.MinItem()
retry:
	parent, curr, found := s.Skiplist.Lookup(itm, s.cmp, ctx.buf, ctx.slSts)
	// Page has been removed already
	if !found || PageId(curr) != pid {
		return
	}

	pPid := PageId(parent)
	pPg, err := s.ReadPage(pPid, ctx.pgRdrFn, false, ctx)
	if err != nil {
		panic(err)
	}

	if pPg.NeedRemoval() {
		goto retry
	}

	// Parent might have got a split
	if pPg.Next() != pid {
		goto retry
	}

	var pgBuf = ctx.GetBuffer(bufEncPage)
	var metaBuf = ctx.GetBuffer(bufEncMeta)
	var fdSz, staleFdSz int

	s.tryPageSwapin(pg)
	pPg.Merge(pg)

	var offsets []LSSOffset
	var wbufs [][]byte
	var res LSSResource

	if s.shouldPersist {
		var numSegments int
		metaBuf = marshalPageSMO(pg, metaBuf)
		pgBuf, fdSz, staleFdSz, numSegments = pPg.Marshal(pgBuf, FullMarshal)

		sizes := []int{
			lssBlockTypeSize + len(metaBuf),
			lssBlockTypeSize + len(pgBuf),
		}

		offsets, wbufs, res = s.lss.ReserveSpaceMulti(sizes)

		writeLSSBlock(wbufs[0], lssPageRemove, metaBuf)

		writeLSSBlock(wbufs[1], lssPageData, pgBuf)
		pPg.AddFlushRecord(offsets[1], fdSz, numSegments)
	}

	if s.UpdateMapping(pPid, pPg, ctx) {
		s.unindexPage(pid, ctx)

		if s.shouldPersist {
			ctx.sts.FlushDataSz += int64(fdSz) - int64(staleFdSz)
			s.lss.FinalizeWrite(res)
		}

		return

	} else if s.shouldPersist {
		discardLSSBlock(wbufs[0])
		discardLSSBlock(wbufs[1])
		s.lss.FinalizeWrite(res)
	}

	goto retry
}

func (s *Plasma) isStartPage(pid PageId) bool {
	return pid.(*skiplist.Node) == s.Skiplist.HeadNode()
}

func (s *Plasma) StartPageId() PageId {
	return s.Skiplist.HeadNode()
}

func (s *Plasma) EndPageId() PageId {
	return s.Skiplist.TailNode()
}

func (s *Plasma) trySMOs(pid PageId, pg Page, ctx *wCtx, doUpdate bool) bool {
	var updated bool

	if pg.NeedCompaction(s.Config.MaxDeltaChainLen) {
		staleFdSz := pg.Compact()
		if updated = s.UpdateMapping(pid, pg, ctx); updated {
			ctx.sts.Compacts++
			ctx.sts.FlushDataSz -= int64(staleFdSz)
		} else {
			ctx.sts.CompactConflicts++
		}
	} else if pg.NeedSplit(s.Config.MaxPageItems) {
		splitPid := s.AllocPageId(ctx)

		var fdSz, splitFdSz, staleFdSz, numSegments, numSegmentsSplit int
		var pgBuf = ctx.GetBuffer(bufEncPage)
		var splitPgBuf = ctx.GetBuffer(bufEncMeta)

		newPg := pg.Split(splitPid)

		// Skip split, but compact
		if newPg == nil {
			s.FreePageId(splitPid, ctx)
			staleFdSz := pg.Compact()
			if updated = s.UpdateMapping(pid, pg, ctx); updated {
				ctx.sts.FlushDataSz -= int64(staleFdSz)
			}
			return updated
		}

		var offsets []LSSOffset
		var wbufs [][]byte
		var res LSSResource

		// Replace one page with two pages
		if s.shouldPersist {
			pgBuf, fdSz, staleFdSz, numSegments = pg.Marshal(pgBuf, s.Config.MaxPageLSSSegments)
			splitPgBuf, splitFdSz, _, numSegmentsSplit = newPg.Marshal(splitPgBuf, 1)

			sizes := []int{
				lssBlockTypeSize + len(pgBuf),
				lssBlockTypeSize + len(splitPgBuf),
			}

			offsets, wbufs, res = s.lss.ReserveSpaceMulti(sizes)

			typ := pgFlushLSSType(pg, numSegments)
			writeLSSBlock(wbufs[0], typ, pgBuf)
			pg.AddFlushRecord(offsets[0], fdSz, numSegments)

			writeLSSBlock(wbufs[1], lssPageData, splitPgBuf)
			newPg.AddFlushRecord(offsets[1], splitFdSz, numSegmentsSplit)
		}

		s.CreateMapping(splitPid, newPg, ctx)
		if updated = s.UpdateMapping(pid, pg, ctx); updated {
			s.indexPage(splitPid, ctx)
			ctx.sts.Splits++

			if s.shouldPersist {
				ctx.sts.FlushDataSz += int64(fdSz) + int64(splitFdSz) - int64(staleFdSz)
				s.lss.FinalizeWrite(res)
			}
		} else {
			ctx.sts.SplitConflicts++
			s.FreePageId(splitPid, ctx)

			if s.shouldPersist {
				discardLSSBlock(wbufs[0])
				discardLSSBlock(wbufs[1])
				s.lss.FinalizeWrite(res)
			}
		}
	} else if !s.isStartPage(pid) && pg.NeedMerge(s.Config.MinPageItems) {
		pg.Close()
		if updated = s.UpdateMapping(pid, pg, ctx); updated {
			s.tryPageRemoval(pid, pg, ctx)
			ctx.sts.Merges++
		} else {
			ctx.sts.MergeConflicts++
		}
	} else if doUpdate {
		updated = s.UpdateMapping(pid, pg, ctx)
	}

	return updated
}

func (s *Plasma) tryThrottleForMemory(ctx *wCtx) {
	if s.hasMemoryPressure {
		for s.TriggerSwapper(ctx.SwapperContext()) {
			time.Sleep(swapperWaitInterval)
		}
	}
}

func (s *Plasma) fetchPage(itm unsafe.Pointer, ctx *wCtx) (pid PageId, pg Page, err error) {
retry:
	if prev, curr, found := s.Skiplist.Lookup(itm, s.cmp, ctx.buf, ctx.slSts); found {
		pid = curr
	} else {
		pid = prev
	}

refresh:
	s.tryThrottleForMemory(ctx)

	if pg, err = s.ReadPage(pid, ctx.pgRdrFn, false, ctx); err != nil {
		return nil, nil, err
	}

	if !pg.InRange(itm) {
		pid = pg.Next()
		goto refresh
	}

	if pg.NeedRemoval() {
		s.tryPageRemoval(pid, pg, ctx)
		goto retry
	}

	s.updateCacheMeta(pid)

	return
}

func (w *Writer) Insert(itm unsafe.Pointer) error {
retry:
	pid, pg, err := w.fetchPage(itm, w.wCtx)
	if err != nil {
		return err
	}

	nr := w.sts.NumLSSReads
	pg.Insert(itm)

	if !w.trySMOs(pid, pg, w.wCtx, true) {
		w.sts.InsertConflicts++
		goto retry
	}

	w.sts.BytesIncoming += int64(w.itemSize(itm))
	w.sts.Inserts++
	if w.sts.NumLSSReads-nr > 0 {
		w.sts.CacheMisses++
	} else {
		w.sts.CacheHits++
	}

	w.trySMRObjects(w.wCtx, writerSMRBufferSize)
	return nil
}

func (w *Writer) Delete(itm unsafe.Pointer) error {
retry:
	pid, pg, err := w.fetchPage(itm, w.wCtx)
	if err != nil {
		return err
	}

	nr := w.sts.NumLSSReads
	pg.Delete(itm)

	if !w.trySMOs(pid, pg, w.wCtx, true) {
		w.sts.DeleteConflicts++
		goto retry
	}
	w.sts.BytesIncoming += int64(w.itemSize(itm))
	w.sts.Deletes++
	if w.sts.NumLSSReads-nr > 0 {
		w.sts.CacheMisses++
	} else {
		w.sts.CacheHits++
	}

	w.trySMRObjects(w.wCtx, writerSMRBufferSize)
	return nil
}

func (w *Writer) Lookup(itm unsafe.Pointer) (unsafe.Pointer, error) {
	pid, pg, err := w.fetchPage(itm, w.wCtx)
	if err != nil {
		return nil, err
	}

	nr := w.sts.NumLSSReads
	ret := pg.Lookup(itm)
	w.trySMOs(pid, pg, w.wCtx, false)
	if w.sts.NumLSSReads-nr > 0 {
		w.sts.CacheMisses++
	} else {
		w.sts.CacheHits++
	}

	return ret, nil
}

func (s *Plasma) fetchPageFromLSS(baseOffset LSSOffset, ctx *wCtx) (*page, error) {
	return s.fetchPageFromLSS2(baseOffset, ctx, ctx.pgAllocCtx, ctx.storeCtx)
}

func (s *Plasma) fetchPageFromLSS2(baseOffset LSSOffset, ctx *wCtx,
	aCtx *allocCtx, sCtx *storeCtx) (*page, error) {
	pg := newPage2(nil, nil, ctx, sCtx, aCtx).(*page)
	offset := baseOffset
	data := ctx.GetBuffer(bufFetch)
	numSegments := 0
loop:
	for {
		l, err := s.lss.Read(offset, data)
		if err != nil {
			return nil, err
		}

		ctx.sts.NumLSSReads++
		ctx.sts.LSSReadBytes += int64(l)

		typ := getLSSBlockType(data)
		switch typ {
		case lssPageData, lssPageReloc, lssPageUpdate:
			currPgDelta := newPage2(nil, nil, ctx, sCtx, aCtx).(*page)
			data := data[lssBlockTypeSize:l]
			nextOffset, hasChain := currPgDelta.unmarshalDelta(data, ctx)
			currPgDelta.AddFlushRecord(offset, len(data), 1)
			pg.Append(currPgDelta)
			offset = nextOffset
			numSegments++

			if !hasChain {
				break loop
			}
		default:
			panic(fmt.Sprintf("Invalid page data type %d", typ))
		}
	}

	if pg.head != nil {
		pg.SetNumSegments(numSegments)
		pg.head.rightSibling = pg.getPageId(pg.head.hiItm, ctx)
	}

	return pg, nil
}

func (s *Plasma) logError(err string) {
	fmt.Printf("Plasma: (fatal error - %s)\n", err)
}

func (w *Writer) CompactAll() {
	callb := func(pid PageId, partn RangePartition) error {
		if pg, err := w.ReadPage(pid, nil, false, w.wCtx); err == nil {
			staleFdSz := pg.Compact()
			if updated := w.UpdateMapping(pid, pg, w.wCtx); updated {
				w.wCtx.sts.FlushDataSz -= int64(staleFdSz)
			}
		}
		return nil
	}

	w.PageVisitor(callb, 1)
}

func SetMemoryQuota(m int64) {
	atomic.StoreInt64(&memQuota, m)
}

func MemoryInUse() (sz int64) {
	buf := dbInstances.MakeBuf()
	defer dbInstances.FreeBuf(buf)

	ctx := dbInstances.NewIterator(ComparePlasma, buf)
	return MemoryInUse2(ctx)
}

func MemoryInUse2(ctx SwapperContext) (sz int64) {
	iter := (*skiplist.Iterator)(ctx)
	for iter.SeekFirst(); iter.Valid(); iter.Next() {
		db := (*Plasma)(iter.Get())
		sz += db.MemoryInUse()
	}

	return
}

func (s *Plasma) tryPageSwapin(pg Page) bool {
	var ok bool
	pgi := pg.(*page)
	if pgi.head != nil && pgi.head.state.IsEvicted() {
		pw := newPgDeltaWalker(pgi.head, pgi.ctx)
		// Force the pagewalker to read the swapout delta
		for !pw.End() {
			if pw.Op() == opSwapoutDelta {
				pw.Next()
				break
			}
		}
		ok = pw.SwapIn(pgi)
		pw.Close()
	}

	return ok
}

func (s *Plasma) ItemsCount() int64 {
	return s.itemsCount
}
