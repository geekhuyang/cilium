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

package metricsmap

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"unsafe"

	"github.com/cilium/cilium/pkg/bpf"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/metrics"
	monitorAPI "github.com/cilium/cilium/pkg/monitor/api"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	// Metrics is the bpf metrics map
	Metrics      *bpf.Map
	log          = logging.DefaultLogger.WithField(logfields.LogSubsys, "map-metrics")
	possibleCpus int
)

const (
	// MapName for metrics map.
	MapName = "cilium_metrics"
	// MaxEntries is the maximum number of keys that can be present in the
	// Metrics Map.
	MaxEntries = 65536
	// dirIngress and dirEgress values should match with
	// METRIC_INGRESS and METRIC_EGRESS in bpf/lib/common.h
	dirIngress = 1
	dirEgress  = 2
	dirUnknown = 0

	possibleCPUSysfsPath = "/sys/devices/system/cpu/possible"
)

// direction is the metrics direction i.e ingress (to an endpoint)
// or egress (from an endpoint). If it's none of the above, we return
// UNKNOWN direction.
var direction = map[uint8]string{
	0: "UNKNOWN",
	1: "INGRESS",
	2: "EGRESS",
}

type pad3uint16 [3]uint16

// DeepCopyInto is a deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *pad3uint16) DeepCopyInto(out *pad3uint16) {
	copy(out[:], in[:])
	return
}

// Key must be in sync with struct metrics_key in <bpf/lib/common.h>
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=github.com/cilium/cilium/pkg/bpf.MapKey
type Key struct {
	Reason   uint8      `align:"reason"`
	Dir      uint8      `align:"dir"`
	Reserved pad3uint16 `align:"reserved"`
}

// Value must be in sync with struct metrics_value in <bpf/lib/common.h>
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=github.com/cilium/cilium/pkg/bpf.MapValue
type Value struct {
	Count uint64 `align:"count"`
	Bytes uint64 `align:"bytes"`
}

// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=github.com/cilium/cilium/pkg/bpf.MapValue
// Values is a slice of Values
type Values []Value

// DeepCopyMapValue is an autogenerated deepcopy function, copying the receiver, creating a new bpf.MapValue.
func (vs *Values) DeepCopyMapValue() bpf.MapValue {
	if c := vs.DeepCopy(); c != nil {
		return &c
	}
	return nil
}

// String converts the value into a human readable string format
func (vs Values) String() string {
	sumCount, sumBytes := uint64(0), uint64(0)
	for _, v := range vs {
		sumCount += v.Count
		sumBytes += v.Bytes
	}
	return fmt.Sprintf("count:%d bytes:%d", sumCount, sumBytes)
}

// GetValuePtr returns the unsafe pointer to the BPF value.
func (vs *Values) GetValuePtr() unsafe.Pointer {
	return unsafe.Pointer(vs)
}

// String converts the key into a human readable string format
func (k *Key) String() string {
	return fmt.Sprintf("reason:%d dir:%d", k.Reason, k.Dir)
}

// MetricDirection gets the direction in human readable string format
func MetricDirection(dir uint8) string {
	switch dir {
	case dirIngress:
		return direction[dir]
	case dirEgress:
		return direction[dir]
	}
	return direction[dirUnknown]
}

// Direction gets the direction in human readable string format
func (k *Key) Direction() string {
	return MetricDirection(k.Dir)
}

// DropForwardReason gets the forwarded/dropped reason in human readable string format
func (k *Key) DropForwardReason() string {
	return monitorAPI.DropReason(k.Reason)
}

// GetKeyPtr returns the unsafe pointer to the BPF key
func (k *Key) GetKeyPtr() unsafe.Pointer { return unsafe.Pointer(k) }

// String converts the value into a human readable string format
func (v *Value) String() string {
	return fmt.Sprintf("count:%d bytes:%d", v.Count, v.Bytes)
}

// RequestCount returns the drop/forward count in a human readable string format
func (v *Value) RequestCount() string {
	return strconv.FormatUint(v.Count, 10)
}

// RequestBytes returns drop/forward bytes in a human readable string format
func (v *Value) RequestBytes() string {
	return strconv.FormatUint(v.Bytes, 10)
}

// IsDrop checks if the reason is drop or not.
func (k *Key) IsDrop() bool {
	return k.Reason == monitorAPI.DropInvalid || k.Reason >= monitorAPI.DropMin
}

// CountFloat converts the request count to float
func (v *Value) CountFloat() float64 {
	return float64(v.Count)
}

// bytesFloat converts the bytes count to float
func (v *Value) bytesFloat() float64 {
	return float64(v.Bytes)
}

// NewValue returns a new empty instance of the structure representing the BPF
// map value
func (k *Key) NewValue() bpf.MapValue { return &Value{} }

// GetValuePtr returns the unsafe pointer to the BPF value.
func (v *Value) GetValuePtr() unsafe.Pointer {
	return unsafe.Pointer(v)
}

func updateMetric(getCounter func() (prometheus.Counter, error), newValue float64) {
	counter, err := getCounter()
	if err != nil {
		log.WithError(err).Warn("Failed to update prometheus metrics")
		return
	}

	oldValue := metrics.GetCounterValue(counter)
	if newValue > oldValue {
		counter.Add((newValue - oldValue))
	}
}

// updatePrometheusMetrics checks the metricsmap key value pair
// and determines which prometheus metrics along with respective labels
// need to be updated.
func updatePrometheusMetrics(key *Key, val *Value) {
	updateMetric(func() (prometheus.Counter, error) {
		if key.IsDrop() {
			return metrics.DropCount.GetMetricWithLabelValues(key.DropForwardReason(), key.Direction())
		}
		return metrics.ForwardCount.GetMetricWithLabelValues(key.Direction())
	}, val.CountFloat())

	updateMetric(func() (prometheus.Counter, error) {
		if key.IsDrop() {
			return metrics.DropBytes.GetMetricWithLabelValues(key.DropForwardReason(), key.Direction())
		}
		return metrics.ForwardBytes.GetMetricWithLabelValues(key.Direction())
	}, val.bytesFloat())
}

// SyncMetricsMap is called periodically to sync off the metrics map by
// aggregating it into drops (by drop reason and direction) and
// forwards (by direction) with the prometheus server.
func SyncMetricsMap(ctx context.Context) error {
	entry := make([]Value, possibleCpus)
	file := bpf.MapPath(MapName)
	metricsmap, err := bpf.OpenMap(file)

	if err != nil {
		return fmt.Errorf("unable to open metrics map: %s", err)
	}
	defer metricsmap.Close()

	var key, nextKey Key
	for {
		err := bpf.GetNextKey(metricsmap.GetFd(), unsafe.Pointer(&key), unsafe.Pointer(&nextKey))
		if err != nil {
			break
		}
		err = bpf.LookupElement(metricsmap.GetFd(), unsafe.Pointer(&nextKey), unsafe.Pointer(&entry[0]))
		if err != nil {
			return fmt.Errorf("unable to lookup metrics map: %s", err)
		}

		// cannot use `range entry` since, if the first value for a particular
		// CPU is zero, it never iterates over the next non-zero value.
		for i := 0; i < possibleCpus; i++ {
			// Increment Prometheus metrics here.
			updatePrometheusMetrics(&nextKey, &entry[i])
		}
		key = nextKey

	}
	return nil
}

// getNumPossibleCPUs returns a total number of possible CPUS, i.e. CPUs that
// have been allocated resources and can be brought online if they are present.
// The number is retrieved by parsing /sys/device/system/cpu/possible.
//
// See https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git/tree/include/linux/cpumask.h?h=v4.19#n50
// for more details.
func getNumPossibleCPUs() int {
	f, err := os.Open(possibleCPUSysfsPath)
	if err != nil {
		log.WithError(err).Errorf("unable to open %q", possibleCPUSysfsPath)
	}
	defer f.Close()

	return getNumPossibleCPUsFromReader(f)
}

func getNumPossibleCPUsFromReader(r io.Reader) int {
	out, err := ioutil.ReadAll(r)
	if err != nil {
		log.WithError(err).Errorf("unable to read %q to get CPU count", possibleCPUSysfsPath)
		return 0
	}

	var start, end int
	count := 0
	for _, s := range strings.Split(string(out), ",") {
		// Go's scanf will return an error if a format cannot be fully matched.
		// So, just ignore it, as a partial match (e.g. when there is only one
		// CPU) is expected.
		n, err := fmt.Sscanf(s, "%d-%d", &start, &end)

		switch n {
		case 0:
			log.WithError(err).Errorf("failed to scan %q to retrieve number of possible CPUs!", s)
			return 0
		case 1:
			count++
		default:
			count += (end - start + 1)
		}
	}

	return count
}

func init() {
	possibleCpus = getNumPossibleCPUs()

	vs := make(Values, possibleCpus)

	// Metrics is a mapping of all packet drops and forwards associated with
	// the node on ingress/egress direction
	Metrics = bpf.NewPerCPUHashMap(
		MapName,
		&Key{},
		int(unsafe.Sizeof(Key{})),
		&vs,
		int(unsafe.Sizeof(Value{})),
		possibleCpus,
		MaxEntries,
		0, 0,
		bpf.ConvertKeyValue,
	)
}
