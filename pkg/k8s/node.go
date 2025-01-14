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

package k8s

import (
	"encoding/json"
	"fmt"
	"net"

	"github.com/cilium/cilium/pkg/annotation"
	"github.com/cilium/cilium/pkg/cidr"
	"github.com/cilium/cilium/pkg/k8s/types"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/node"
	"github.com/cilium/cilium/pkg/node/addressing"
	"github.com/cilium/cilium/pkg/option"

	"github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ParseNodeAddressType converts a Kubernetes NodeAddressType to a Cilium
// NodeAddressType. If the Kubernetes NodeAddressType does not have a
// corresponding Cilium AddressType, returns an error.
func ParseNodeAddressType(k8sAddress v1.NodeAddressType) (addressing.AddressType, error) {

	var err error
	convertedAddr := addressing.AddressType(k8sAddress)

	switch convertedAddr {
	case addressing.NodeExternalDNS, addressing.NodeExternalIP, addressing.NodeHostName, addressing.NodeInternalIP, addressing.NodeInternalDNS:
	default:
		err = fmt.Errorf("invalid Kubernetes NodeAddressType %s", convertedAddr)
	}
	return convertedAddr, err
}

// ParseNode parses a kubernetes node to a cilium node
func ParseNode(k8sNode *types.Node, source node.Source) *node.Node {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.NodeName:  k8sNode.Name,
		logfields.K8sNodeID: k8sNode.UID,
	})
	addrs := []node.Address{}
	for _, addr := range k8sNode.StatusAddresses {
		// We only care about this address types,
		// we ignore all other types.
		switch addr.Type {
		case v1.NodeInternalIP, v1.NodeExternalIP:
		default:
			continue
		}
		// If the address is not set let's not parse it at all.
		// This can be the case for v1.NodeExternalIPs
		if addr.Address == "" {
			continue
		}
		ip := net.ParseIP(addr.Address)
		if ip == nil {
			scopedLog.WithFields(logrus.Fields{
				logfields.IPAddr: addr.Address,
				"type":           addr.Type,
			}).Warn("Ignoring invalid node IP")
			continue
		}

		addressType, err := ParseNodeAddressType(addr.Type)

		if err != nil {
			scopedLog.WithError(err).Warn("invalid address type for node")
		}

		na := node.Address{
			Type: addressType,
			IP:   ip,
		}
		addrs = append(addrs, na)
	}

	k8sNodeAddHostIP := func(annotation string) {
		if ciliumInternalIP, ok := k8sNode.Annotations[annotation]; !ok || ciliumInternalIP == "" {
			scopedLog.Debugf("Missing %s. Annotation required when IPSec Enabled", annotation)
		} else if ip := net.ParseIP(ciliumInternalIP); ip == nil {
			scopedLog.Debugf("ParseIP %s error", ciliumInternalIP)
		} else {
			na := node.Address{
				Type: addressing.NodeCiliumInternalIP,
				IP:   ip,
			}
			addrs = append(addrs, na)
			scopedLog.Debugf("Add NodeCiliumInternalIP: %s", ip)
		}
	}

	k8sNodeAddHostIP(annotation.CiliumHostIP)
	k8sNodeAddHostIP(annotation.CiliumHostIPv6)

	newNode := &node.Node{
		Name:        k8sNode.Name,
		Cluster:     option.Config.ClusterName,
		IPAddresses: addrs,
		Source:      source,
	}

	if len(k8sNode.SpecPodCIDR) != 0 {
		if allocCIDR, err := cidr.ParseCIDR(k8sNode.SpecPodCIDR); err != nil {
			scopedLog.WithError(err).WithField(logfields.V4Prefix, k8sNode.SpecPodCIDR).Warn("Invalid PodCIDR value for node")
		} else {
			if allocCIDR.IP.To4() != nil {
				newNode.IPv4AllocCIDR = allocCIDR
			} else {
				newNode.IPv6AllocCIDR = allocCIDR
			}
		}
	}
	// Spec.PodCIDR takes precedence since it's
	// the CIDR assigned by k8s controller manager
	// In case it's invalid or empty then we fall back to our annotations.
	if newNode.IPv4AllocCIDR == nil {
		if ipv4CIDR, ok := k8sNode.Annotations[annotation.V4CIDRName]; !ok || ipv4CIDR == "" {
			scopedLog.Debug("Empty IPv4 CIDR annotation in node")
		} else {
			allocCIDR, err := cidr.ParseCIDR(ipv4CIDR)
			if err != nil {
				scopedLog.WithError(err).WithField(logfields.V4Prefix, ipv4CIDR).Error("BUG, invalid IPv4 annotation CIDR in node")
			} else {
				newNode.IPv4AllocCIDR = allocCIDR
			}
		}
	}

	if newNode.IPv6AllocCIDR == nil {
		if ipv6CIDR, ok := k8sNode.Annotations[annotation.V6CIDRName]; !ok || ipv6CIDR == "" {
			scopedLog.Debug("Empty IPv6 CIDR annotation in node")
		} else {
			allocCIDR, err := cidr.ParseCIDR(ipv6CIDR)
			if err != nil {
				scopedLog.WithError(err).WithField(logfields.V6Prefix, ipv6CIDR).Error("BUG, invalid IPv6 annotation CIDR in node")
			} else {
				newNode.IPv6AllocCIDR = allocCIDR
			}
		}
	}

	if newNode.IPv4HealthIP == nil {
		if healthIP, ok := k8sNode.Annotations[annotation.V4HealthName]; !ok || healthIP == "" {
			scopedLog.Debug("Empty IPv4 health endpoint annotation in node")
		} else if ip := net.ParseIP(healthIP); ip == nil {
			scopedLog.WithField(logfields.V4HealthIP, healthIP).Error("BUG, invalid IPv4 health endpoint annotation in node")
		} else {
			newNode.IPv4HealthIP = ip
		}
	}

	if newNode.IPv6HealthIP == nil {
		if healthIP, ok := k8sNode.Annotations[annotation.V6HealthName]; !ok || healthIP == "" {
			scopedLog.Debug("Empty IPv6 health endpoint annotation in node")
		} else if ip := net.ParseIP(healthIP); ip == nil {
			scopedLog.WithField(logfields.V6HealthIP, healthIP).Error("BUG, invalid IPv6 health endpoint annotation in node")
		} else {
			newNode.IPv6HealthIP = ip
		}
	}

	return newNode
}

// GetNode returns the kubernetes nodeName's node information from the
// kubernetes api server
func GetNode(c kubernetes.Interface, nodeName string) (*v1.Node, error) {
	// Try to retrieve node's cidr and addresses from k8s's configuration
	return c.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
}

// SetNodeNetworkUnavailableFalse sets Kubernetes NodeNetworkUnavailable to
// false as Cilium is managing the network connectivity.
// https://kubernetes.io/docs/concepts/architecture/nodes/#condition
func SetNodeNetworkUnavailableFalse(c kubernetes.Interface, nodeName string) error {
	condition := v1.NodeCondition{
		Type:               v1.NodeNetworkUnavailable,
		Status:             v1.ConditionFalse,
		Reason:             "CiliumIsUp",
		Message:            "Cilium is running on this node",
		LastTransitionTime: metav1.Now(),
		LastHeartbeatTime:  metav1.Now(),
	}
	raw, err := json.Marshal(&[]v1.NodeCondition{condition})
	if err != nil {
		return err
	}
	patch := []byte(fmt.Sprintf(`{"status":{"conditions":%s}}`, raw))
	_, err = c.CoreV1().Nodes().PatchStatus(nodeName, patch)
	return err
}
