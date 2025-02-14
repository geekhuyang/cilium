// Copyright 2016-2019 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package allocator

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/cilium/cilium/pkg/backoff"
	"github.com/cilium/cilium/pkg/idpool"
	"github.com/cilium/cilium/pkg/kvstore"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/uuid"

	"github.com/sirupsen/logrus"
)

var (
	log = logging.DefaultLogger.WithField(logfields.LogSubsys, "allocator")
)

const (
	// maxAllocAttempts is the number of attempted allocation requests
	// performed before failing.
	maxAllocAttempts = 16

	// listTimeout is the time to wait for the initial list operation to
	// succeed when creating a new allocator
	listTimeout = 3 * time.Minute
)

// Allocator is a distributed ID allocator backed by a KVstore. It maps
// arbitrary keys to identifiers. Multiple users on different cluster nodes can
// in parallel request the ID for keys and are guaranteed to retrieve the same
// ID for an identical key.
//
// Slave keys:
//   Slave keys are owned by individual nodes:
//     - basePath/value/key1/node1 => 1001
//     - basePath/value/key1/node2 => 1001
//     - basePath/value/key2/node1 => 1002
//     - basePath/value/key2/node2 => 1002
//
//   If at least one key exists with the prefix basePath/value/keyN then that
//   key must be considered to be in use in the allocation space.
//
//   Slave keys are protected by a lease and will automatically get removed
//   after ~ option.Config.KVstoreLeaseTTL if the node does not renew in time.
//
// Master key:
//    - basePath/id/1001 => key1
//    - basePath/id/1002 => key2
//
//   Master keys provide the mapping from ID to key. As long as a master key
//   for an ID exists, the ID is still in use. However, if a master key is no
//   longer backed by at least one slave key, the garbage collector will
//   eventually release the master key and return it back to the pool.
//
// Lookup ID by key:
// 1. Return ID from local cache updated by watcher (no kvstore interactions)
// 2. Do ListPrefix() on slave key excluding node suffix, return the first
//    result that matches the exact prefix.
//
// Lookup key by ID:
// 1. Return key from local cache updated by watcher (no kvstore interactions)
// 2. Do Get() on master key, return result
//
// Allocate:
// 1. Check local key cache, increment, and return if key is already in use
//    locally (no kvstore interactions)
// 2. Check local cache updated by watcher, if...
//
// ... match found:
// 2.1 Create a new slave key. This operation is potentially racy as the master
//     key can be removed in the meantime.
//       etcd: Create is made conditional on existence of master key
//       consul: locking
//
// ... match not found:
// 2.1 Select new unused id from local cache
// 2.2 Create a new master key with the condition that it may not exist
// 2.3 Create a new slave key
//
// 1.1. If found, increment and return (no kvstore interactions)
// 2. Lookup ID by key in local cache or via first slave key found in kvstore
// 2.1
// 3.
//
// Release:
//  1. Reduce local reference count until last use (no kvstore interactions)
//  2. Delete slave key (basePath/value/key1/node1)
//     This automatically guarantees that when the last node has relesed the
//     key, the key is no longer found by Get()
//  3. If the node goes down, all slave keys of that node are removed after
//     the TTL expires (auto release).
type Allocator struct {
	// events is a channel which will receive AllocatorEvent as IDs are
	// added, modified or removed from the allocator
	events AllocatorEventChan

	// keyType is an instance of the type to be used as allocator key.
	keyType AllocatorKey

	// basePrefix is the prefix in the kvstore that all keys share which
	// are being managed by this allocator. The basePrefix typically
	// consists of something like: "space/project/allocatorName"
	basePrefix string

	// idPrefix is the kvstore key prefix for all master keys. It is being
	// derived from the basePrefix.
	idPrefix string

	// valuePrefix is the kvstore key prefix for all slave keys. It is
	// being derived from the basePrefix.
	valuePrefix string

	// slaveKeysMutex protects the concurrent access of the slave key by this
	// agent.
	slaveKeysMutex lock.Mutex

	// lockPrefix is the prefix to use for all kvstore locks. This prefix
	// is different from the idPrefix and valuePrefix to simplify watching
	// for ID and key changes.
	lockPrefix string

	// min is the lower limit when allocating IDs. The allocator will never
	// allocate an ID lesser than this value.
	min idpool.ID

	// max is the upper limit when allocating IDs. The allocator will never
	// allocate an ID greater than this value.
	max idpool.ID

	// prefixMask if set, will be ORed to all selected IDs prior to
	// allocation
	prefixMask idpool.ID

	// localKeys contains all keys including their reference count for keys
	// which have been allocated and are in local use
	localKeys *localKeys

	// suffix is the suffix attached to keys which must be node specific,
	// this is typical set to the node's IP address
	suffix string

	// lockless is true if allocation can be done lockless. This depends on
	// the underlying kvstore backend
	lockless bool

	// backoffTemplate is the backoff configuration while allocating
	backoffTemplate backoff.Exponential

	// mainCache is the main cache, representing the allocator contents of
	// the primary kvstore connection
	mainCache cache

	// remoteCachesMutex protects accesse to remoteCaches
	remoteCachesMutex lock.RWMutex

	// remoteCaches is the list of additional remote caches being watched
	// in addition to the main cache
	remoteCaches map[*RemoteCache]struct{}

	// stopGC is the channel used to stop the garbage collector
	stopGC chan struct{}

	// initialListDone is a channel that is closed when the initial
	// synchronization has completed
	initialListDone waitChan

	// idPool maintains a pool of available ids for allocation.
	idPool *idpool.IDPool

	// enableMasterKeyProtection if true, causes master keys that are still in
	// local use to be automatically re-created
	enableMasterKeyProtection bool

	// disableGC disables the garbage collector
	disableGC bool
}

func locklessCapability() bool {
	required := kvstore.CapabilityCreateIfExists | kvstore.CapabilityDeleteOnZeroCount
	return kvstore.GetCapabilities()&required == required
}

// AllocatorOption is the base type for allocator options
type AllocatorOption func(*Allocator)

// NewAllocatorForGC returns an allocator  that can be used to run RunGC()
func NewAllocatorForGC(basePath string) *Allocator {
	return &Allocator{
		idPrefix:    path.Join(basePath, "id"),
		valuePrefix: path.Join(basePath, "value"),
		lockPrefix:  path.Join(basePath, "locks"),
	}
}

// NewAllocator creates a new Allocator. Any type can be used as key as long as
// the type implements the AllocatorKey interface. A variable of the type has
// to be passed into NewAllocator() to make the type known.  The specified base
// path is used to prefix all keys in the kvstore. The provided path must be
// unique.
//
// The allocator can be configured by passing in additional options:
//  - WithEvents() - enable Events channel
//  - WithSuffix(string) - customize the node specifix suffix to attach to keys
//  - WithMin(id) - minimum ID to allocate (default: 1)
//  - WithMax(id) - maximum ID to allocate (default max(uint64))
//
// After creation, IDs can be allocated with Allocate() and released with
// Release()
func NewAllocator(basePath string, typ AllocatorKey, opts ...AllocatorOption) (*Allocator, error) {
	if kvstore.Client() == nil {
		return nil, fmt.Errorf("kvstore client not configured")
	}

	a := &Allocator{
		keyType:      typ,
		basePrefix:   basePath,
		idPrefix:     path.Join(basePath, "id"),
		valuePrefix:  path.Join(basePath, "value"),
		lockPrefix:   path.Join(basePath, "locks"),
		min:          idpool.ID(1),
		max:          idpool.ID(^uint64(0)),
		localKeys:    newLocalKeys(),
		stopGC:       make(chan struct{}),
		suffix:       uuid.NewUUID().String()[:10],
		lockless:     locklessCapability(),
		remoteCaches: map[*RemoteCache]struct{}{},
		backoffTemplate: backoff.Exponential{
			Min:    time.Duration(20) * time.Millisecond,
			Factor: 2.0,
		},
	}

	for _, fn := range opts {
		fn(a)
	}

	a.mainCache = newCache(kvstore.Client(), a.idPrefix)

	// invalid prefixes are only deleted from the main cache
	a.mainCache.deleteInvalidPrefixes = true

	if a.suffix == "<nil>" {
		return nil, errors.New("allocator suffix is <nil> and unlikely unique")
	}

	if a.min < 1 {
		return nil, errors.New("minimum ID must be >= 1")
	}

	if a.max <= a.min {
		return nil, errors.New("maximum ID must be greater than minimum ID")
	}

	a.idPool = idpool.NewIDPool(a.min, a.max)

	a.initialListDone = a.mainCache.start(a)
	if !a.disableGC {
		go func() {
			select {
			case <-a.initialListDone:
			case <-time.After(listTimeout):
				log.Fatalf("Timeout while waiting for initial allocator state")
			}
			a.startLocalKeySync()
		}()
	}

	return a, nil
}

// WithEvents enables receiving of events.
//
// CAUTION: When using this function. The provided channel must be continuously
// read while NewAllocator() is being called to ensure that the channel does
// not block indefinitely while NewAllocator() emits events on it while
// populating the initial cache.
func WithEvents(events AllocatorEventChan) AllocatorOption {
	return func(a *Allocator) { a.events = events }
}

// WithSuffix sets the suffix of the allocator to the specified value
func WithSuffix(v string) AllocatorOption {
	return func(a *Allocator) { a.suffix = v }
}

// WithMin sets the minimum identifier to be allocated
func WithMin(id idpool.ID) AllocatorOption {
	return func(a *Allocator) { a.min = id }
}

// WithMax sets the maximum identifier to be allocated
func WithMax(id idpool.ID) AllocatorOption {
	return func(a *Allocator) { a.max = id }
}

// WithPrefixMask sets the prefix used for all ID allocations. If set, the mask
// will be ORed to all selected IDs prior to allocation. It is the
// responsibility of the caller to ensure that the mask is not conflicting with
// min..max.
func WithPrefixMask(mask idpool.ID) AllocatorOption {
	return func(a *Allocator) { a.prefixMask = mask }
}

// WithMasterKeyProtection will watch for delete events on master keys and
// re-created them if local usage suggests that the key is still in use
func WithMasterKeyProtection() AllocatorOption {
	return func(a *Allocator) { a.enableMasterKeyProtection = true }
}

// WithoutGC disables the use of the garbage collector
func WithoutGC() AllocatorOption {
	return func(a *Allocator) { a.disableGC = true }
}

// Delete deletes an allocator and stops the garbage collector
func (a *Allocator) Delete() {
	close(a.stopGC)
	a.mainCache.stop()

	if a.events != nil {
		close(a.events)
	}
}

// WaitForInitialSync waits until the initial sync is complete
func (a *Allocator) WaitForInitialSync(ctx context.Context) error {
	select {
	case <-a.initialListDone:
	case <-ctx.Done():
		return fmt.Errorf("identity sync with kvstore was cancelled: %s", ctx.Err())
	}

	return nil
}

// lockPath locks a key in the scope of an allocator
func (a *Allocator) lockPath(ctx context.Context, key string) (*kvstore.Lock, error) {
	suffix := strings.TrimPrefix(key, a.basePrefix)
	return kvstore.LockPath(ctx, path.Join(a.lockPrefix, suffix))
}

// DeleteAllKeys will delete all keys
func (a *Allocator) DeleteAllKeys() {
	kvstore.DeletePrefix(a.basePrefix)
}

// RangeFunc is the function called by RangeCache
type RangeFunc func(idpool.ID, AllocatorKey)

// ForeachCache iterates over the allocator cache and calls RangeFunc on each
// cached entry
func (a *Allocator) ForeachCache(cb RangeFunc) {
	a.mainCache.foreach(cb)

	a.remoteCachesMutex.RLock()
	for rc := range a.remoteCaches {
		rc.cache.foreach(cb)
	}
	a.remoteCachesMutex.RUnlock()
}

// Selects an available ID.
// Returns a triple of the selected ID ORed with prefixMask,
// the ID string and the originally selected ID.
func (a *Allocator) selectAvailableID() (idpool.ID, string, idpool.ID) {
	if id := a.idPool.LeaseAvailableID(); id != idpool.NoID {
		unmaskedID := id
		id |= a.prefixMask
		return id, id.String(), unmaskedID
	}

	return 0, "", 0
}

func (a *Allocator) createValueNodeKey(ctx context.Context, key string, newID idpool.ID, lock kvstore.KVLocker) error {
	// add a new key /value/<key>/<node> to account for the reference
	// The key is protected with a TTL/lease and will expire after LeaseTTL
	valueKey := path.Join(a.valuePrefix, key, a.suffix)
	if _, err := kvstore.UpdateIfDifferentIfLocked(ctx, valueKey, []byte(newID.String()), true, lock); err != nil {
		return fmt.Errorf("unable to create value-node key '%s': %s", valueKey, err)
	}

	// mark the key as verified in the local cache
	if err := a.localKeys.verify(key); err != nil {
		log.WithError(err).Error("BUG: Unable to verify local key")
	}

	return nil
}

// AllocatorKey is the interface to implement in order for a type to be used as
// key for the allocator
type AllocatorKey interface {
	// GetKey must return the key in string representation
	GetKey() string

	// PutKey must transform the key in string representation back into its
	// original type
	PutKey(v string) (AllocatorKey, error)

	// String must return the key in human readable string representation
	String() string
}

func (a *Allocator) lockedAllocate(ctx context.Context, key AllocatorKey) (idpool.ID, bool, error) {
	kvstore.Trace("Allocating key in kvstore", nil, logrus.Fields{fieldKey: key})

	k := key.GetKey()
	lock, err := a.lockPath(ctx, k)
	if err != nil {
		return 0, false, err
	}

	defer lock.Unlock()

	// fetch first key that matches /value/<key> while ignoring the
	// node suffix
	value, err := a.GetIfLocked(ctx, key, lock)
	if err != nil {
		return 0, false, err
	}

	kvstore.Trace("kvstore state is: ", nil, logrus.Fields{fieldID: value})

	a.slaveKeysMutex.Lock()
	defer a.slaveKeysMutex.Unlock()

	// We shouldn't assume the fact the master key does not exist in the kvstore
	// that localKeys does not have it. The KVStore might have lost all of its
	// data but the local agent still holds a reference for the given master key.
	if value == 0 {
		value = a.localKeys.lookupKey(k)
		if value != 0 {
			// re-create master key
			keyPath := path.Join(a.idPrefix, strconv.FormatUint(uint64(value), 10))
			success, err := kvstore.CreateOnlyIfLocked(ctx, keyPath, []byte(k), false, lock)
			if err != nil || !success {
				return 0, false, fmt.Errorf("unable to create master key '%s': %s", keyPath, err)
			}
		}
	} else {
		_, err := a.localKeys.allocate(k, value)
		if err != nil {
			return 0, false, fmt.Errorf("unable to reserve local key '%s': %s", k, err)
		}
	}
	if value != 0 {
		if err = a.createValueNodeKey(ctx, k, value, lock); err != nil {
			a.localKeys.release(k)
			return 0, false, fmt.Errorf("unable to create slave key '%s': %s", k, err)
		}

		log.WithField(fieldKey, k).Info("Reusing existing global key")

		return value, false, nil
	}

	id, strID, unmaskedID := a.selectAvailableID()
	if id == 0 {
		return 0, false, fmt.Errorf("no more available IDs in configured space")
	}

	kvstore.Trace("Selected available key", nil, logrus.Fields{fieldID: id})

	releaseKeyAndID := func() {
		a.localKeys.release(k)
		a.idPool.Release(unmaskedID)
	}

	oldID, err := a.localKeys.allocate(k, id)
	if err != nil {
		a.idPool.Release(unmaskedID)
		return 0, false, fmt.Errorf("unable to reserve local key '%s': %s", k, err)
	}

	// Another local writer beat us to allocating an ID for the same key,
	// start over
	if id != oldID {
		releaseKeyAndID()
		return 0, false, fmt.Errorf("another writer has allocated this key")
	}

	// create /id/<ID> and fail if it already exists
	keyPath := path.Join(a.idPrefix, strID)
	success, err := kvstore.CreateOnlyIfLocked(ctx, keyPath, []byte(k), false, lock)
	if err != nil || !success {
		// Creation failed. Another agent most likely beat us to allocating this
		// ID, retry.
		releaseKeyAndID()
		return 0, false, fmt.Errorf("unable to create master key '%s': %s", keyPath, err)
	}

	// Notify pool that leased ID is now in-use.
	a.idPool.Use(unmaskedID)

	if err = a.createValueNodeKey(ctx, k, id, lock); err != nil {
		// We will leak the master key here as the key has already been
		// exposed and may be in use by other nodes. The garbage
		// collector will release it again.
		releaseKeyAndID()
		return 0, false, fmt.Errorf("slave key creation failed '%s': %s", k, err)
	}

	log.WithField(fieldKey, k).Info("Allocated new global key")

	return id, true, nil
}

// Allocate will retrieve the ID for the provided key. If no ID has been
// allocated for this key yet, a key will be allocated. If allocation fails,
// most likely due to a parallel allocation of the same ID by another user,
// allocation is re-attempted for maxAllocAttempts times.
//
// Returns the ID allocated to the key, if the ID had to be allocated, then
// true is returned. An error is returned in case of failure.
func (a *Allocator) Allocate(ctx context.Context, key AllocatorKey) (idpool.ID, bool, error) {
	var (
		err   error
		value idpool.ID
		isNew bool
		k     = key.GetKey()
	)

	log.WithField(fieldKey, key).Debug("Allocating key")

	select {
	case <-a.initialListDone:
	case <-ctx.Done():
		return 0, false, fmt.Errorf("allocation was cancelled while waiting for initial key list to be received: %s", ctx.Err())
	}

	// Check our list of local keys already in use and increment the
	// refcnt. The returned key must be released afterwards. No kvstore
	// operation was performed for this allocation
	if val := a.localKeys.use(k); val != idpool.NoID {
		kvstore.Trace("Reusing local id", nil, logrus.Fields{fieldID: val, fieldKey: key})
		a.mainCache.insert(key, val)
		return val, false, nil
	}

	kvstore.Trace("Allocating from kvstore", nil, logrus.Fields{fieldKey: key})

	// make a copy of the template and customize it
	boff := a.backoffTemplate
	boff.Name = key.String()

	for attempt := 0; attempt < maxAllocAttempts; attempt++ {
		// FIXME: Add non-locking variant
		value, isNew, err = a.lockedAllocate(ctx, key)
		if err == nil {
			a.mainCache.insert(key, value)
			log.WithField(fieldKey, key).WithField(fieldID, value).Debug("Allocated key")
			return value, isNew, nil
		}

		scopedLog := log.WithFields(logrus.Fields{
			fieldKey:          key,
			logfields.Attempt: attempt,
		})

		select {
		case <-ctx.Done():
			scopedLog.WithError(ctx.Err()).Warning("Ongoing key allocation has been cancelled")
			return 0, false, fmt.Errorf("key allocation cancelled: %s", ctx.Err())
		default:
			scopedLog.WithError(err).Warning("Key allocation attempt failed")
		}

		if waitErr := boff.Wait(ctx); waitErr != nil {
			return 0, false, waitErr
		}
	}

	return 0, false, err
}

// GetIfLocked returns the ID which is allocated to a key. Returns an ID of NoID if no ID
// has been allocated to this key yet if the client is still holding the given
// lock.
func (a *Allocator) GetIfLocked(ctx context.Context, key AllocatorKey, lock kvstore.KVLocker) (idpool.ID, error) {
	if id := a.mainCache.get(key.GetKey()); id != idpool.NoID {
		return id, nil
	}

	return a.GetNoCacheIfLocked(ctx, key, lock)
}

// Get returns the ID which is allocated to a key. Returns an ID of NoID if no ID
// has been allocated to this key yet.
func (a *Allocator) Get(ctx context.Context, key AllocatorKey) (idpool.ID, error) {
	if id := a.mainCache.get(key.GetKey()); id != idpool.NoID {
		return id, nil
	}

	return a.GetNoCache(ctx, key)
}

func prefixMatchesKey(prefix, key string) bool {
	// cilium/state/identities/v1/value/label;foo;bar;/172.0.124.60
	lastSlash := strings.LastIndex(key, "/")
	return len(prefix) == lastSlash
}

// GetNoCacheIfLocked returns the ID which is allocated to a key in the kvstore
// if the client is still holding the given lock.
func (a *Allocator) GetNoCacheIfLocked(ctx context.Context, key AllocatorKey, lock kvstore.KVLocker) (idpool.ID, error) {
	// ListPrefixIfLocked() will return all keys matching the prefix, the prefix
	// can cover multiple different keys, example:
	//
	// key1 := label1;label2;
	// key2 := label1;label2;label3;
	//
	// In order to retrieve the correct key, the position of the last '/'
	// is significant, e.g.
	//
	// prefix := cilium/state/identities/v1/value/label;foo;
	//
	// key1 := cilium/state/identities/v1/value/label;foo;/172.0.124.60
	// key2 := cilium/state/identities/v1/value/label;foo;bar;/172.0.124.60
	//
	// Only key1 should match
	prefix := path.Join(a.valuePrefix, key.GetKey())
	pairs, err := kvstore.ListPrefixIfLocked(prefix, lock)
	kvstore.Trace("ListPrefixLocked", err, logrus.Fields{fieldPrefix: prefix, "entries": len(pairs)})
	if err != nil {
		return 0, err
	}

	for k, v := range pairs {
		if prefixMatchesKey(prefix, k) {
			id, err := strconv.ParseUint(string(v.Data), 10, 64)
			if err == nil {
				return idpool.ID(id), nil
			}
		}
	}

	return idpool.NoID, nil
}

// GetNoCache returns the ID which is allocated to a key in the kvstore
func (a *Allocator) GetNoCache(ctx context.Context, key AllocatorKey) (idpool.ID, error) {
	// ListPrefix() will return all keys matching the prefix, the prefix
	// can cover multiple different keys, example:
	//
	// key1 := label1;label2;
	// key2 := label1;label2;label3;
	//
	// In order to retrieve the correct key, the position of the last '/'
	// is significant, e.g.
	//
	// prefix := cilium/state/identities/v1/value/label;foo;
	//
	// key1 := cilium/state/identities/v1/value/label;foo;/172.0.124.60
	// key2 := cilium/state/identities/v1/value/label;foo;bar;/172.0.124.60
	//
	// Only key1 should match
	prefix := path.Join(a.valuePrefix, key.GetKey())
	pairs, err := kvstore.ListPrefix(prefix)
	kvstore.Trace("ListPrefix", err, logrus.Fields{fieldPrefix: prefix, "entries": len(pairs)})
	if err != nil {
		return 0, err
	}

	for k, v := range pairs {
		if prefixMatchesKey(prefix, k) {
			id, err := strconv.ParseUint(string(v.Data), 10, 64)
			if err == nil {
				return idpool.ID(id), nil
			}
		}
	}

	return idpool.NoID, nil
}

// GetByID returns the key associated with an ID. Returns nil if no key is
// associated with the ID.
func (a *Allocator) GetByID(id idpool.ID) (AllocatorKey, error) {
	if key := a.mainCache.getByID(id); key != nil {
		return key, nil
	}

	v, err := kvstore.Get(path.Join(a.idPrefix, id.String()))
	if err != nil {
		return nil, err
	}

	return a.keyType.PutKey(string(v))
}

// Release releases the use of an ID associated with the provided key. After
// the last user has released the ID, the key is removed in the KVstore and
// the returned lastUse value is true.
func (a *Allocator) Release(ctx context.Context, key AllocatorKey) (lastUse bool, err error) {
	log.WithField(fieldKey, key).Info("Releasing key")

	select {
	case <-a.initialListDone:
	case <-ctx.Done():
		return false, fmt.Errorf("release was cancelled while waiting for initial key list to be received: %s", ctx.Err())
	}

	k := key.GetKey()

	a.slaveKeysMutex.Lock()
	defer a.slaveKeysMutex.Unlock()

	// release the key locally, if it was the last use, remove the node
	// specific value key to remove the global reference mark
	lastUse, err = a.localKeys.release(k)
	if err != nil {
		return
	}

	if lastUse {
		valueKey := path.Join(a.valuePrefix, k, a.suffix)
		log.WithField(fieldKey, key).Info("Released last local use of key, invoking global release")

		// does not need to be deleted with a lock as its protected by the
		// a.slaveKeysMutex
		if err := kvstore.Delete(valueKey); err != nil {
			log.WithError(err).WithFields(logrus.Fields{fieldKey: key}).Warning("Ignoring node specific ID")
		}

		// if a.lockless {
		// FIXME: etcd 3.3 will make it possible to do a lockless
		// cleanup of the ID and release it right away. For now we rely
		// on the GC to kick in a release unused IDs.
		// }
	}

	return
}

// RunGC scans the kvstore for unused master keys and removes them
func (a *Allocator) RunGC(staleKeysPrevRound map[string]uint64) (map[string]uint64, error) {
	// fetch list of all /id/ keys
	allocated, err := kvstore.ListPrefix(a.idPrefix)
	if err != nil {
		return nil, fmt.Errorf("list failed: %s", err)
	}

	staleKeys := map[string]uint64{}

	// iterate over /id/
	for key, v := range allocated {
		// if a.lockless {
		// FIXME: Add DeleteOnZeroCount support
		// }

		lock, err := a.lockPath(context.Background(), key)
		if err != nil {
			log.WithError(err).WithField(fieldKey, key).Warning("allocator garbage collector was unable to lock key")
			continue
		}

		// fetch list of all /value/<key> keys
		valueKeyPrefix := path.Join(a.valuePrefix, string(v.Data))
		pairs, err := kvstore.ListPrefixIfLocked(valueKeyPrefix, lock)
		if err != nil {
			log.WithError(err).WithField(fieldPrefix, valueKeyPrefix).Warning("allocator garbage collector was unable to list keys")
			lock.Unlock()
			continue
		}

		hasUsers := false
		for k := range pairs {
			if prefixMatchesKey(valueKeyPrefix, k) {
				hasUsers = true
				break
			}
		}

		// if ID has no user, delete it
		if !hasUsers {
			scopedLog := log.WithFields(logrus.Fields{
				fieldKey: key,
				fieldID:  path.Base(key),
			})
			// Only delete if this key was previously marked as to be deleted
			if modRev, ok := staleKeysPrevRound[key]; ok && modRev == v.ModRevision {
				if err := kvstore.DeleteIfLocked(key, lock); err != nil {
					scopedLog.WithError(err).Warning("Unable to delete unused allocator master key")
				} else {
					scopedLog.Info("Deleted unused allocator master key")
				}
			} else {
				// If the key was not found mark it to be delete in the next RunGC
				staleKeys[key] = v.ModRevision
			}
		}

		lock.Unlock()
	}

	return staleKeys, nil
}

func (a *Allocator) recreateMasterKey(id idpool.ID, value string, reliablyMissing bool) {
	var (
		err       error
		recreated bool
		keyPath   = path.Join(a.idPrefix, id.String())
		valueKey  = path.Join(a.valuePrefix, value, a.suffix)
	)

	if reliablyMissing {
		recreated, err = kvstore.CreateOnly(context.TODO(), keyPath, []byte(value), false)
	} else {
		recreated, err = kvstore.UpdateIfDifferent(context.TODO(), keyPath, []byte(value), false)
	}
	switch {
	case err != nil:
		log.WithError(err).WithField(fieldKey, keyPath).Warning("Unable to re-create missing master key")
	case recreated:
		log.WithField(fieldKey, keyPath).Warning("Re-created missing master key")
	}

	// Also re-create the slave key in case it has been deleted. This will
	// ensure that the next garbage collection cycle of any participating
	// node does not remove the master key again.
	if reliablyMissing {
		recreated, err = kvstore.CreateOnly(context.TODO(), valueKey, []byte(id.String()), true)
	} else {
		recreated, err = kvstore.UpdateIfDifferent(context.TODO(), valueKey, []byte(id.String()), true)
	}
	switch {
	case err != nil:
		log.WithError(err).WithField(fieldKey, valueKey).Warning("Unable to re-create missing slave key")
	case recreated:
		log.WithField(fieldKey, valueKey).Warning("Re-created missing slave key")
	}
}

// syncLocalKeys checks the kvstore and verifies that a master key exists for
// all locally used allocations. This will restore master keys if deleted for
// some reason.
func (a *Allocator) syncLocalKeys() error {
	// Create a local copy of all local allocations to not require to hold
	// any locks while performing kvstore operations. Local use can
	// disappear while we perform the sync but that is fine as worst case,
	// a master key is created for a slave key that no longer exists. The
	// garbage collector will remove it again.
	ids := a.localKeys.getVerifiedIDs()

	for id, value := range ids {
		a.recreateMasterKey(id, value, false)
	}

	return nil
}

func (a *Allocator) startLocalKeySync() {
	go func(a *Allocator) {
		for {
			if err := a.syncLocalKeys(); err != nil {
				log.WithError(err).WithFields(logrus.Fields{fieldPrefix: a.idPrefix}).
					Warning("Unable to run local key sync routine")
			}

			select {
			case <-a.stopGC:
				log.WithFields(logrus.Fields{fieldPrefix: a.idPrefix}).
					Debug("Stopped master key sync routine")
				return
			case <-time.After(option.Config.KVstorePeriodicSync):
			}
		}
	}(a)
}

// AllocatorEventChan is a channel to receive allocator events on
type AllocatorEventChan chan AllocatorEvent

// AllocatorEvent is an event sent over AllocatorEventChan
type AllocatorEvent struct {
	// Typ is the type of event (create / modify / delete)
	Typ kvstore.EventType

	// ID is the allocated ID
	ID idpool.ID

	// Key is the key associated with the ID
	Key AllocatorKey
}

// RemoteCache represents the cache content of an additional kvstore managing
// identities. The contents are not directly accessible but will be merged into
// the ForeachCache() function.
type RemoteCache struct {
	cache     cache
	allocator *Allocator
}

// WatchRemoteKVStore starts watching an allocator base prefix the kvstore
// represents by the provided backend. A local cache of all identities of that
// kvstore will be maintained in the RemoteCache structure returned and will
// start being reported in the identities returned by the ForeachCache()
// function.
func (a *Allocator) WatchRemoteKVStore(backend kvstore.BackendOperations, prefix string) *RemoteCache {
	rc := &RemoteCache{
		cache:     newCache(backend, path.Join(prefix, "id")),
		allocator: a,
	}

	a.remoteCachesMutex.Lock()
	a.remoteCaches[rc] = struct{}{}
	a.remoteCachesMutex.Unlock()

	rc.cache.start(a)

	return rc
}

// Close stops watching for identities in the kvstore associated with the
// remote cache and will clear the local cache.
func (rc *RemoteCache) Close() {
	rc.allocator.remoteCachesMutex.Lock()
	delete(rc.allocator.remoteCaches, rc)
	rc.allocator.remoteCachesMutex.Unlock()

	rc.cache.stop()
}
