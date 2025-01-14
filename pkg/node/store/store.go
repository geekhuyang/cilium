// Copyright 2018-2019 Authors of Cilium
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

package store

import (
	"path"
	"time"

	"github.com/cilium/cilium/pkg/defaults"
	"github.com/cilium/cilium/pkg/identity"
	"github.com/cilium/cilium/pkg/ipcache"
	"github.com/cilium/cilium/pkg/kvstore"
	"github.com/cilium/cilium/pkg/kvstore/store"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/node"
	"github.com/cilium/cilium/pkg/option"
)

var (
	// NodeStorePrefix is the kvstore prefix of the shared store
	//
	// WARNING - STABLE API: Changing the structure or values of this will
	// break backwards compatibility
	NodeStorePrefix = path.Join(kvstore.BaseKeyPrefix, "state", "nodes", "v1")

	// KeyCreator creates a node for a shared store
	KeyCreator = func() store.Key {
		n := node.Node{}
		return &n
	}

	log = logging.DefaultLogger.WithField(logfields.LogSubsys, "node-store")
)

// NodeObserver implements the store.Observer interface and delegates update
// and deletion events to the node object itself.
type NodeObserver struct {
	manager NodeManager
}

// NewNodeObserver returns a new NodeObserver associated with the specified
// node manager
func NewNodeObserver(manager NodeManager) *NodeObserver {
	return &NodeObserver{manager: manager}
}

func (o *NodeObserver) OnUpdate(k store.Key) {
	if n, ok := k.(*node.Node); ok {
		nodeCopy := n.DeepCopy()
		nodeCopy.Source = node.FromKVStore
		o.manager.NodeUpdated(*nodeCopy)

		ciliumIPv4 := nodeCopy.GetCiliumInternalIP(false)
		if ciliumIPv4 != nil {
			hostIP := nodeCopy.GetNodeIP(false)
			hostKey := node.GetIPsecKeyIdentity()
			ipcache.IPIdentityCache.Upsert(ciliumIPv4.String(), hostIP, hostKey, ipcache.Identity{
				ID:     identity.ReservedIdentityHost,
				Source: ipcache.FromKVStore,
			})
		}

		if option.Config.EncryptNode {
			hostIP := nodeCopy.GetNodeIP(false)
			if hostIP != nil {
				hostKey := node.GetIPsecKeyIdentity()
				ipcache.IPIdentityCache.Upsert(hostIP.String(), hostIP, hostKey, ipcache.Identity{
					ID:     identity.ReservedIdentityHost,
					Source: ipcache.FromKVStore,
				})
			}
		}

		ciliumIPv6 := nodeCopy.GetCiliumInternalIP(true)
		if ciliumIPv6 != nil {
			hostIP := nodeCopy.GetNodeIP(true)
			hostKey := node.GetIPsecKeyIdentity()
			ipcache.IPIdentityCache.Upsert(ciliumIPv6.String(), hostIP, hostKey, ipcache.Identity{
				ID:     identity.ReservedIdentityHost,
				Source: ipcache.FromKVStore,
			})
		}
	}
}

func (o *NodeObserver) OnDelete(k store.NamedKey) {
	if n, ok := k.(*node.Node); ok {
		nodeCopy := n.DeepCopy()
		nodeCopy.Source = node.FromKVStore

		go func() {
			time.Sleep(defaults.NodeDeleteDelay)

			if o.manager.Exists(nodeCopy.Identity()) {
				log.Warningf("Received node delete event for node %s which re-appeared within %s",
					nodeCopy.Name, defaults.NodeDeleteDelay)
				return
			}

			o.manager.NodeDeleted(*nodeCopy)

			ciliumIPv4 := nodeCopy.GetCiliumInternalIP(false)
			if ciliumIPv4 != nil {
				ipcache.IPIdentityCache.Delete(ciliumIPv4.String(), ipcache.FromKVStore)
			}
			ciliumIPv6 := nodeCopy.GetCiliumInternalIP(true)
			if ciliumIPv6 != nil {
				ipcache.IPIdentityCache.Delete(ciliumIPv6.String(), ipcache.FromKVStore)
			}
		}()
	}
}

// NodeRegistrar is a wrapper around store.SharedStore.
type NodeRegistrar struct {
	*store.SharedStore
}

// NodeManager is the interface that the manager of nodes has to implement
type NodeManager interface {
	// NodeSoftUpdated is called when the store detects a change in the
	// node that does not require datapath updates.
	NodeSoftUpdated(n node.Node)

	// NodeUpdated is called when the store detects a change in node
	// information
	NodeUpdated(n node.Node)

	// NodeDeleted is called when the store detects a deletion of a node
	NodeDeleted(n node.Node)

	// Exists is called to verify if a node exists
	Exists(id node.Identity) bool
}

// RegisterNode registers the local node in the cluster
func (nr *NodeRegistrar) RegisterNode(n *node.Node, manager NodeManager) error {

	// Join the shared store holding node information of entire cluster
	store, err := store.JoinSharedStore(store.Configuration{
		Prefix:     NodeStorePrefix,
		KeyCreator: KeyCreator,
		Observer:   NewNodeObserver(manager),
	})

	if err != nil {
		return err
	}

	if err = store.UpdateLocalKeySync(n); err != nil {
		store.Release()
		return err
	}

	nr.SharedStore = store

	return nil
}

// UpdateLocalKeySync synchronizes the local key for the node using the
// SharedStore.
func (nr *NodeRegistrar) UpdateLocalKeySync(n *node.Node) error {
	return nr.SharedStore.UpdateLocalKeySync(n)
}
