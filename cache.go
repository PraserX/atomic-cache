package atomiccache

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emirpasic/gods/trees/btree"
)

// Internal cache errors
var (
	ErrNotFound   = errors.New("Record not found")
	ErrDataLimit  = errors.New("Can't create new record, it violates data limit")
	ErrFullMemory = errors.New("Can't create new rocord, memory is full")
)

// Constans below are used for shard section identification.
const (
	// SMSH - Small Shards section
	SMSH = iota + 1
	// MDSH - Medium Shards section
	MDSH
	// LGSH - Large Shards section
	LGSH
)

// AtomicCache structure represents whole cache memory.
type AtomicCache struct {
	// RWMutex is used for access to shards array.
	sync.RWMutex

	// Lookup structure used for global index. It is based on BTree structure.
	lookup *btree.Tree

	// Shards lookup tables which contains information about shards sections.
	smallShards, mediumShards, largeShards ShardsLookup

	// Size of byte array used for memory allocation at small shard section.
	RecordSizeSmall uint32
	// Size of byte array used for memory allocation at medium shard section.
	RecordSizeMedium uint32
	// Size of byte array used for memory allocation at large shard section.
	RecordSizeLarge uint32

	// Maximum records per shard.
	MaxRecords uint32

	// Maximum small shards which can be allocated in cache memory.
	MaxShardsSmall uint32
	// Maximum medium shards which can be allocated in cache memory.
	MaxShardsMedium uint32
	// Maximum large shards which can be allocated in cache memory.
	MaxShardsLarge uint32

	// Garbage collector starter (run garbage collection every X memory sets).
	GcStarter uint32
	// Garbage collector counter for starter.
	GcCounter uint32

	// Buffer contains all unattended cache set requests. It has a maximum site
	// which is equal to MaxRecords value.
	buffer []BufferItem
}

// ShardsLookup represents data structure for for each shards section. In each
// section we have different size of records in that shards.
type ShardsLookup struct {
	// Array of pointers to shard objects.
	shards []*Shard
	// Array of shard indexes which are currently active.
	shardsActive []uint32
	// Array of shard indexes which are currently available for new allocation.
	shardsAvail []uint32
}

// LookupRecord represents item in lookup table. One record contains index of
// shard and record. So we can determine which shard access and which record of
// shard to get. Record also contains expiration time.
type LookupRecord struct {
	RecordIndex  uint32
	ShardIndex   uint32
	ShardSection uint8
	Expiration   time.Time
}

// BufferItem is used for buffer, which contains all unattended cache set
// requrest.
type BufferItem struct {
	Key    []byte
	Data   []byte
	Expire time.Duration
}

// New initialize whole cache memory with one allocated shard.
func New(opts ...Option) *AtomicCache {
	var options = &Options{
		RecordSizeSmall:  512,
		RecordSizeMedium: 2048,
		RecordSizeLarge:  8128,
		MaxRecords:       2048,
		MaxShardsSmall:   256,
		MaxShardsMedium:  128,
		MaxShardsLarge:   64,
		GcStarter:        5000,
	}

	for _, opt := range opts {
		opt(options)
	}

	// Init cache structure
	cache := &AtomicCache{}

	// Init lookup table
	cache.lookup = btree.NewWithStringComparator(3)

	// Init small shards section
	initShardsSection(&cache.smallShards, options.MaxShardsSmall, options.MaxRecords, options.RecordSizeSmall)
	initShardsSection(&cache.mediumShards, options.MaxShardsMedium, options.MaxRecords, options.RecordSizeMedium)
	initShardsSection(&cache.largeShards, options.MaxShardsLarge, options.MaxRecords, options.RecordSizeLarge)

	// Define setup values
	cache.RecordSizeSmall = options.RecordSizeSmall
	cache.RecordSizeMedium = options.RecordSizeMedium
	cache.RecordSizeLarge = options.RecordSizeLarge
	cache.MaxRecords = options.MaxRecords
	cache.MaxShardsSmall = options.MaxShardsSmall
	cache.MaxShardsMedium = options.MaxShardsMedium
	cache.MaxShardsLarge = options.MaxShardsLarge
	cache.GcStarter = options.GcStarter

	return cache
}

// initShardsSection provides shards sections initialization. So the cache has
// one shard in each section at the begging.
func initShardsSection(shardsSection *ShardsLookup, maxShards, maxRecords, recordSize uint32) {
	var shardIndex uint32

	shardsSection.shards = make([]*Shard, maxShards, maxShards)
	for i := uint32(0); i < maxShards; i++ {
		shardsSection.shardsAvail = append(shardsSection.shardsAvail, i)
	}

	shardIndex, shardsSection.shardsAvail = shardsSection.shardsAvail[0], shardsSection.shardsAvail[1:]
	shardsSection.shardsActive = append(shardsSection.shardsActive, shardIndex)
	shardsSection.shards[shardIndex] = NewShard(maxRecords, recordSize)
}

// Set store data to cache memory. If key/record is already in memory, then data
// are replaced. If not, it checks if there are some allocated shard with empty
// space for data. If there is no empty space, new shard is allocated. Otherwise
// some valid record (FIFO queue) is deleted and new one is stored.
func (a *AtomicCache) Set(key []byte, data []byte, expire time.Duration) error {
	if len(data) > int(a.RecordSizeLarge) {
		return ErrDataLimit
	}

	new := false
	shardSection, shardSectionID := a.getShardsSectionBySize(len(data))

	a.Lock()
	if ival, ok := a.lookup.Get(string(key)); !ok {
		new = true
	} else {
		val := ival.(LookupRecord)

		if val.ShardSection != shardSectionID {
			shardSection.shards[val.ShardIndex].Free(val.RecordIndex)
			val.RecordIndex = shardSection.shards[val.ShardIndex].Set(data)
			a.lookup.Put(string(key), LookupRecord{ShardIndex: val.ShardIndex, ShardSection: shardSectionID, RecordIndex: val.RecordIndex, Expiration: a.getExprTime(expire)})
		} else {
			prevShardSection := a.getShardsSectionByID(val.ShardSection)
			prevShardSection.shards[val.ShardIndex].Free(val.RecordIndex)
			new = true
		}
	}

	if new {
		if si, ok := a.getShard(shardSectionID); ok {
			ri := shardSection.shards[si].Set(data)
			a.lookup.Put(string(key), LookupRecord{ShardIndex: si, ShardSection: shardSectionID, RecordIndex: ri, Expiration: a.getExprTime(expire)})
		} else if si, ok := a.getEmptyShard(shardSectionID); ok {
			shardSection.shards[si] = NewShard(a.MaxRecords, a.getRecordSizeByShardSectionID(shardSectionID))
			ri := shardSection.shards[si].Set(data)
			a.lookup.Put(string(key), LookupRecord{ShardIndex: si, ShardSection: shardSectionID, RecordIndex: ri, Expiration: a.getExprTime(expire)})
		} else {
			if len(a.buffer) <= int(a.MaxRecords) {
				a.buffer = append(a.buffer, BufferItem{Key: key, Data: data, Expire: expire})
			} else {
				a.Unlock()
				return ErrFullMemory
			}

			go a.collectGarbage()
		}
	}
	a.Unlock()

	if atomic.AddUint32(&a.GcCounter, 1) == a.GcStarter {
		atomic.StoreUint32(&a.GcCounter, 0)
		go a.collectGarbage()
	}

	return nil
}

// Get returns list of bytes if record is present in cache memory. If record is
// not found, then error is returned and list is nil.
func (a *AtomicCache) Get(key []byte) ([]byte, error) {
	var result []byte
	var hit = false

	a.RLock()
	if ival, ok := a.lookup.Get(string(key)); ok {
		val := ival.(LookupRecord)
		shardSection := a.getShardsSectionByID(val.ShardSection)

		if shardSection.shards[val.ShardIndex] != nil && time.Now().Before(val.Expiration) {
			result = shardSection.shards[val.ShardIndex].Get(val.RecordIndex)
			hit = true
		}
	}
	a.RUnlock()

	if hit {
		return result, nil
	}

	return nil, ErrNotFound
}

// releaseShard release shard if there is no record in memory. It returns true
// if shard was released. The function requires the shard section ID and
// shard ID on input.
// This method is not thread safe and additional locks are required.
func (a *AtomicCache) releaseShard(shardSectionID uint8, shard uint32) bool {
	var shardSection *ShardsLookup

	if shardSection = a.getShardsSectionByID(shardSectionID); shardSection == nil {
		return false
	}

	if shardSection.shards[shard].IsEmpty() == true {
		shardSection.shards[shard] = nil
		return true
	}

	return false
}

// getShard return index of shard which have some available space for new
// record. If there is no shard with available space, then false is returned as
// a second value. The function requires the shard section ID on input.
// This method is not thread safe and additional locks are required.
func (a *AtomicCache) getShard(shardSectionID uint8) (uint32, bool) {
	var shardSection *ShardsLookup

	if shardSection = a.getShardsSectionByID(shardSectionID); shardSection == nil {
		return 0, false
	}

	for _, shardIndex := range shardSection.shardsActive {
		if shardSection.shards[shardIndex].GetSlotsAvail() != 0 {
			return shardIndex, true
		}
	}

	return 0, false
}

// getEmptyShard return index of shard that can be used for new shard
// allocation. If there is no left index, then false is returned as a second
// value. The function requires the shard section ID on input.
// This method is not thread safe and additional locks are required.
func (a *AtomicCache) getEmptyShard(shardSectionID uint8) (uint32, bool) {
	var shardSection *ShardsLookup

	if shardSection = a.getShardsSectionByID(shardSectionID); shardSection == nil {
		return 0, false
	}

	if len(shardSection.shardsAvail) == 0 {
		return 0, false
	}

	var shardIndex uint32
	shardIndex, shardSection.shardsAvail = shardSection.shardsAvail[0], shardSection.shardsAvail[1:]

	return shardIndex, true
}

// getShardsSectionBySize returns shards section lookup structure and section
// identifier as a second value. The function requires the data size value on
// input. If data are bigger than allowed value, then nil and 0 is returned.
// This method is not thread safe and additional locks are required.
func (a *AtomicCache) getShardsSectionBySize(dataSize int) (*ShardsLookup, uint8) {
	if dataSize <= int(a.RecordSizeSmall) {
		return &a.smallShards, uint8(SMSH)
	} else if dataSize > int(a.RecordSizeSmall) && dataSize <= int(a.RecordSizeMedium) {
		return &a.mediumShards, uint8(MDSH)
	} else if dataSize > int(a.RecordSizeMedium) && dataSize <= int(a.RecordSizeLarge) {
		return &a.largeShards, uint8(LGSH)
	}

	return nil, 0
}

// getShardsSectionByID returns shards section lookup structure. The function
// requires the shard section ID on input. If section ID is not valid, nil
// is returned.
// This method is not thread safe and additional locks are required.
func (a *AtomicCache) getShardsSectionByID(sectionID uint8) *ShardsLookup {
	if sectionID == uint8(SMSH) {
		return &a.smallShards
	} else if sectionID == uint8(MDSH) {
		return &a.mediumShards
	} else if sectionID == uint8(LGSH) {
		return &a.largeShards
	}

	return nil
}

// getRecordSizeByShardSectionID returns maximum record size for specified
// shard section ID. It returns 0 if there is not known section ID on input.
// This method is not thread safe and additional locks are required.
func (a *AtomicCache) getRecordSizeByShardSectionID(sectionID uint8) uint32 {
	if sectionID == SMSH {
		return a.RecordSizeSmall
	} else if sectionID == MDSH {
		return a.RecordSizeMedium
	} else if sectionID == LGSH {
		return a.RecordSizeLarge
	}

	return 0
}

// getExprTime return expiration time based on duration. If duration is 0, then
// maximum expiration time is used (48 hours).
func (a *AtomicCache) getExprTime(expire time.Duration) time.Time {
	if expire == 0 {
		return time.Now().Add(48 * time.Hour)
	}

	return time.Now().Add(expire)
}

// collectGarbage provides garbage collect. It goes throught lookup table and
// checks expiration time. If shard end up empty, then garbage collect release
// him, but only if there is more than one shard in charge (we always have one
// active shard).
func (a *AtomicCache) collectGarbage() {
	a.Lock()
	for _, k := range a.lookup.Keys() {
		iv, _ := a.lookup.Get(k.(string))                      // get record
		v := iv.(LookupRecord)                                 // convert record from interface to LookupRecord
		shardSection := a.getShardsSectionByID(v.ShardSection) // get shard section
		if time.Now().After(v.Expiration) {
			shardSection.shards[v.ShardIndex].Free(v.RecordIndex)
			if len(shardSection.shardsActive) > 1 {
				a.releaseShard(v.ShardSection, v.ShardIndex)
			}
			a.lookup.Remove(k)
		}
	}

	var bi BufferItem
	for x := 0; x < len(a.buffer); x++ {
		bi, a.buffer = a.buffer[0], a.buffer[1:]
		if err := a.Set(bi.Key, bi.Data, bi.Expire); err != nil {
			break
		}
	}
	a.Unlock()
}
