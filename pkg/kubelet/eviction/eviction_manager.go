/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package eviction

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/golang/glog"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientv1 "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/api/v1"
	v1qos "k8s.io/kubernetes/pkg/api/v1/helper/qos"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/server/stats"
	kubelettypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
)

const (
	podCleanupTimeout  = 30 * time.Second
	podCleanupPollFreq = time.Second
)

// managerImpl implements Manager
type managerImpl struct {
	//  used to track time
	clock clock.Clock
	// config is how the manager is configured
	config Config
	// the function to invoke to kill a pod
	killPodFunc KillPodFunc
	// the interface that knows how to do image gc
	imageGC ImageGC
	// protects access to internal state
	sync.RWMutex
	// node conditions are the set of conditions present
	nodeConditions []v1.NodeConditionType
	// captures when a node condition was last observed based on a threshold being met
	nodeConditionsLastObservedAt nodeConditionsObservedAt
	// nodeRef is a reference to the node
	nodeRef *clientv1.ObjectReference
	// used to record events about the node
	recorder record.EventRecorder
	// used to measure usage stats on system
	summaryProvider stats.SummaryProvider
	// records when a threshold was first observed
	thresholdsFirstObservedAt thresholdsObservedAt
	// records the set of thresholds that have been met (including graceperiod) but not yet resolved
	thresholdsMet []evictionapi.Threshold
	// resourceToRankFunc maps a resource to ranking function for that resource.
	resourceToRankFunc map[v1.ResourceName]rankFunc
	// resourceToNodeReclaimFuncs maps a resource to an ordered list of functions that know how to reclaim that resource.
	resourceToNodeReclaimFuncs map[v1.ResourceName]nodeReclaimFuncs
	// last observations from synchronize
	lastObservations signalObservations
	// notifiersInitialized indicates if the threshold notifiers have been initialized (i.e. synchronize() has been called once)
	notifiersInitialized bool
	// dedicatedImageFs indicates if imagefs is on a separate device from the rootfs
	dedicatedImageFs *bool
}

// ensure it implements the required interface
var _ Manager = &managerImpl{}

// NewManager returns a configured Manager and an associated admission handler to enforce eviction configuration.
func NewManager(
	summaryProvider stats.SummaryProvider,
	config Config,
	killPodFunc KillPodFunc,
	imageGC ImageGC,
	recorder record.EventRecorder,
	nodeRef *clientv1.ObjectReference,
	clock clock.Clock) (Manager, lifecycle.PodAdmitHandler) {
	manager := &managerImpl{
		clock:           clock,
		killPodFunc:     killPodFunc,
		imageGC:         imageGC,
		config:          config,
		recorder:        recorder,
		summaryProvider: summaryProvider,
		nodeRef:         nodeRef,
		nodeConditionsLastObservedAt: nodeConditionsObservedAt{},
		thresholdsFirstObservedAt:    thresholdsObservedAt{},
		dedicatedImageFs:             nil,
	}
	return manager, manager
}

// Admit rejects a pod if its not safe to admit for node stability.
func (m *managerImpl) Admit(attrs *lifecycle.PodAdmitAttributes) lifecycle.PodAdmitResult {
	m.RLock()
	defer m.RUnlock()
	if len(m.nodeConditions) == 0 {
		return lifecycle.PodAdmitResult{Admit: true}
	}
	// Admit Critical pods even under resource pressure since they are required for system stability.
	// https://github.com/kubernetes/kubernetes/issues/40573 has more details.
	if utilfeature.DefaultFeatureGate.Enabled(features.ExperimentalCriticalPodAnnotation) && kubelettypes.IsCriticalPod(attrs.Pod) {
		return lifecycle.PodAdmitResult{Admit: true}
	}
	// the node has memory pressure, admit if not best-effort
	if hasNodeCondition(m.nodeConditions, v1.NodeMemoryPressure) {
		notBestEffort := v1.PodQOSBestEffort != v1qos.GetPodQOS(attrs.Pod)
		if notBestEffort {
			return lifecycle.PodAdmitResult{Admit: true}
		}
	}

	// reject pods when under memory pressure (if pod is best effort), or if under disk pressure.
	glog.Warningf("Failed to admit pod %s - node has conditions: %v", format.Pod(attrs.Pod), m.nodeConditions)
	return lifecycle.PodAdmitResult{
		Admit:   false,
		Reason:  reason,
		Message: fmt.Sprintf(message, m.nodeConditions),
	}
}

// Start starts the control loop to observe and response to low compute resources.
func (m *managerImpl) Start(diskInfoProvider DiskInfoProvider, podFunc ActivePodsFunc, podCleanedUpFunc PodCleanedUpFunc, nodeProvider NodeProvider, monitoringInterval time.Duration) {
	// start the eviction manager monitoring
	go func() {
		for {
			if evictedPod := m.synchronize(diskInfoProvider, podFunc, nodeProvider); evictedPod != nil {
				glog.Infof("eviction manager: pod %s evicted, waiting for pod to be cleaned up", format.Pod(evictedPod))
				m.waitForPodCleanup(podCleanedUpFunc, evictedPod)
			} else {
				time.Sleep(monitoringInterval)
			}
		}
	}()
}

// IsUnderMemoryPressure returns true if the node is under memory pressure.
func (m *managerImpl) IsUnderMemoryPressure() bool {
	m.RLock()
	defer m.RUnlock()
	return hasNodeCondition(m.nodeConditions, v1.NodeMemoryPressure)
}

// IsUnderDiskPressure returns true if the node is under disk pressure.
func (m *managerImpl) IsUnderDiskPressure() bool {
	m.RLock()
	defer m.RUnlock()
	return hasNodeCondition(m.nodeConditions, v1.NodeDiskPressure)
}

func startMemoryThresholdNotifier(thresholds []evictionapi.Threshold, observations signalObservations, hard bool, handler thresholdNotifierHandlerFunc) error {
	for _, threshold := range thresholds {
		if threshold.Signal != evictionapi.SignalMemoryAvailable || hard != isHardEvictionThreshold(threshold) {
			continue
		}
		observed, found := observations[evictionapi.SignalMemoryAvailable]
		if !found {
			continue
		}
		cgroups, err := cm.GetCgroupSubsystems()
		if err != nil {
			return err
		}
		// TODO add support for eviction from --cgroup-root
		cgpath, found := cgroups.MountPoints["memory"]
		if !found || len(cgpath) == 0 {
			return fmt.Errorf("memory cgroup mount point not found")
		}
		attribute := "memory.usage_in_bytes"
		quantity := evictionapi.GetThresholdQuantity(threshold.Value, observed.capacity)
		usageThreshold := resource.NewQuantity(observed.capacity.Value(), resource.DecimalSI)
		usageThreshold.Sub(*quantity)
		description := fmt.Sprintf("<%s available", formatThresholdValue(threshold.Value))
		memcgThresholdNotifier, err := NewMemCGThresholdNotifier(cgpath, attribute, usageThreshold.String(), description, handler)
		if err != nil {
			return err
		}
		go memcgThresholdNotifier.Start(wait.NeverStop)
		return nil
	}
	return nil
}

// synchronize is the main control loop that enforces eviction thresholds.
// Returns the pod that was killed, or nil if no pod was killed.
func (m *managerImpl) synchronize(diskInfoProvider DiskInfoProvider, podFunc ActivePodsFunc, nodeProvider NodeProvider) *v1.Pod {
	// if we have nothing to do, just return
	thresholds := m.config.Thresholds
	if len(thresholds) == 0 {
		return nil
	}

	glog.V(3).Infof("eviction manager: synchronize housekeeping")
	// build the ranking functions (if not yet known)
	// TODO: have a function in cadvisor that lets us know if global housekeeping has completed
	if m.dedicatedImageFs == nil {
		hasImageFs, ok := diskInfoProvider.HasDedicatedImageFs()
		if ok != nil {
			return nil
		}
		m.dedicatedImageFs = &hasImageFs
		m.resourceToRankFunc = buildResourceToRankFunc(hasImageFs)
		m.resourceToNodeReclaimFuncs = buildResourceToNodeReclaimFuncs(m.imageGC, hasImageFs)

	}

	activePods := podFunc()
	// make observations and get a function to derive pod usage stats relative to those observations.
	observations, statsFunc, err := makeSignalObservations(m.summaryProvider, nodeProvider, activePods, *m.dedicatedImageFs)
	if err != nil {
		glog.Errorf("eviction manager: unexpected err: %v", err)
		return nil
	}
	debugLogObservations("observations", observations)

	// attempt to create a threshold notifier to improve eviction response time
	if m.config.KernelMemcgNotification && !m.notifiersInitialized {
		glog.Infof("eviction manager attempting to integrate with kernel memcg notification api")
		m.notifiersInitialized = true
		// start soft memory notification
		err = startMemoryThresholdNotifier(m.config.Thresholds, observations, false, func(desc string) {
			glog.Infof("soft memory eviction threshold crossed at %s", desc)
			// TODO wait grace period for soft memory limit
			m.synchronize(diskInfoProvider, podFunc, nodeProvider)
		})
		if err != nil {
			glog.Warningf("eviction manager: failed to create hard memory threshold notifier: %v", err)
		}
		// start hard memory notification
		err = startMemoryThresholdNotifier(m.config.Thresholds, observations, true, func(desc string) {
			glog.Infof("hard memory eviction threshold crossed at %s", desc)
			m.synchronize(diskInfoProvider, podFunc, nodeProvider)
		})
		if err != nil {
			glog.Warningf("eviction manager: failed to create soft memory threshold notifier: %v", err)
		}
	}

	// determine the set of thresholds met independent of grace period
	thresholds = thresholdsMet(thresholds, observations, false)
	debugLogThresholdsWithObservation("thresholds - ignoring grace period", thresholds, observations)

	// determine the set of thresholds previously met that have not yet satisfied the associated min-reclaim
	if len(m.thresholdsMet) > 0 {
		thresholdsNotYetResolved := thresholdsMet(m.thresholdsMet, observations, true)
		thresholds = mergeThresholds(thresholds, thresholdsNotYetResolved)
	}
	debugLogThresholdsWithObservation("thresholds - reclaim not satisfied", thresholds, observations)

	// determine the set of thresholds whose stats have been updated since the last sync
	thresholds = thresholdsUpdatedStats(thresholds, observations, m.lastObservations)
	debugLogThresholdsWithObservation("thresholds - updated stats", thresholds, observations)

	// track when a threshold was first observed
	now := m.clock.Now()
	thresholdsFirstObservedAt := thresholdsFirstObservedAt(thresholds, m.thresholdsFirstObservedAt, now)

	// the set of node conditions that are triggered by currently observed thresholds
	nodeConditions := nodeConditions(thresholds)
	if len(nodeConditions) > 0 {
		glog.V(3).Infof("eviction manager: node conditions - observed: %v", nodeConditions)
	}

	// track when a node condition was last observed
	nodeConditionsLastObservedAt := nodeConditionsLastObservedAt(nodeConditions, m.nodeConditionsLastObservedAt, now)

	// node conditions report true if it has been observed within the transition period window
	nodeConditions = nodeConditionsObservedSince(nodeConditionsLastObservedAt, m.config.PressureTransitionPeriod, now)
	if len(nodeConditions) > 0 {
		glog.V(3).Infof("eviction manager: node conditions - transition period not met: %v", nodeConditions)
	}

	// determine the set of thresholds we need to drive eviction behavior (i.e. all grace periods are met)
	thresholds = thresholdsMetGracePeriod(thresholdsFirstObservedAt, now)
	debugLogThresholdsWithObservation("thresholds - grace periods satisified", thresholds, observations)

	// update internal state
	m.Lock()
	m.nodeConditions = nodeConditions
	m.thresholdsFirstObservedAt = thresholdsFirstObservedAt
	m.nodeConditionsLastObservedAt = nodeConditionsLastObservedAt
	m.thresholdsMet = thresholds
	m.lastObservations = observations
	m.Unlock()

	// determine the set of resources under starvation
	starvedResources := getStarvedResources(thresholds)
	if len(starvedResources) == 0 {
		glog.V(3).Infof("eviction manager: no resources are starved")
		return nil
	}

	// rank the resources to reclaim by eviction priority
	sort.Sort(byEvictionPriority(starvedResources))
	resourceToReclaim := starvedResources[0]
	glog.Warningf("eviction manager: attempting to reclaim %v", resourceToReclaim)

	// determine if this is a soft or hard eviction associated with the resource
	softEviction := isSoftEvictionThresholds(thresholds, resourceToReclaim)

	// record an event about the resources we are now attempting to reclaim via eviction
	m.recorder.Eventf(m.nodeRef, v1.EventTypeWarning, "EvictionThresholdMet", "Attempting to reclaim %s", resourceToReclaim)

	// check if there are node-level resources we can reclaim to reduce pressure before evicting end-user pods.
	if m.reclaimNodeLevelResources(resourceToReclaim, observations) {
		glog.Infof("eviction manager: able to reduce %v pressure without evicting pods.", resourceToReclaim)
		return nil
	}

	glog.Infof("eviction manager: must evict pod(s) to reclaim %v", resourceToReclaim)

	// rank the pods for eviction
	rank, ok := m.resourceToRankFunc[resourceToReclaim]
	if !ok {
		glog.Errorf("eviction manager: no ranking function for resource %s", resourceToReclaim)
		return nil
	}

	// the only candidates viable for eviction are those pods that had anything running.
	if len(activePods) == 0 {
		glog.Errorf("eviction manager: eviction thresholds have been met, but no pods are active to evict")
		return nil
	}

	// rank the running pods for eviction for the specified resource
	rank(activePods, statsFunc)

	glog.Infof("eviction manager: pods ranked for eviction: %s", format.Pods(activePods))

	//record age of metrics for met thresholds that we are using for evictions.
	for _, t := range thresholds {
		timeObserved := observations[t.Signal].time
		if !timeObserved.IsZero() {
			metrics.EvictionStatsAge.WithLabelValues(string(t.Signal)).Observe(metrics.SinceInMicroseconds(timeObserved.Time))
		}
	}

	// we kill at most a single pod during each eviction interval
	for i := range activePods {
		pod := activePods[i]
		// If the pod is marked as critical and static, and support for critical pod annotations is enabled,
		// do not evict such pods. Static pods are not re-admitted after evictions.
		// https://github.com/kubernetes/kubernetes/issues/40573 has more details.
		if utilfeature.DefaultFeatureGate.Enabled(features.ExperimentalCriticalPodAnnotation) &&
			kubelettypes.IsCriticalPod(pod) && kubepod.IsStaticPod(pod) {
			continue
		}
		status := v1.PodStatus{
			Phase:   v1.PodFailed,
			Message: fmt.Sprintf(message, resourceToReclaim),
			Reason:  reason,
		}
		// record that we are evicting the pod
		m.recorder.Eventf(pod, v1.EventTypeWarning, reason, fmt.Sprintf(message, resourceToReclaim))
		gracePeriodOverride := int64(0)
		if softEviction {
			gracePeriodOverride = m.config.MaxPodGracePeriodSeconds
		}
		// this is a blocking call and should only return when the pod and its containers are killed.
		err := m.killPodFunc(pod, status, &gracePeriodOverride)
		if err != nil {
			glog.Warningf("eviction manager: error while evicting pod %s: %v", format.Pod(pod), err)
		}
		return pod
	}
	glog.Infof("eviction manager: unable to evict any pods from the node")
	return nil
}

func (m *managerImpl) waitForPodCleanup(podCleanedUpFunc PodCleanedUpFunc, pod *v1.Pod) {
	timeout := m.clock.NewTimer(podCleanupTimeout)
	tick := m.clock.Tick(podCleanupPollFreq)
	for {
		select {
		case <-timeout.C():
			glog.Warningf("eviction manager: timed out waiting for pod %s to be cleaned up", format.Pod(pod))
			return
		case <-tick:
			if podCleanedUpFunc(pod) {
				glog.Infof("eviction manager: pod %s successfully cleaned up", format.Pod(pod))
				return
			}
		}
	}
}

// reclaimNodeLevelResources attempts to reclaim node level resources.  returns true if thresholds were satisfied and no pod eviction is required.
func (m *managerImpl) reclaimNodeLevelResources(resourceToReclaim v1.ResourceName, observations signalObservations) bool {
	nodeReclaimFuncs := m.resourceToNodeReclaimFuncs[resourceToReclaim]
	for _, nodeReclaimFunc := range nodeReclaimFuncs {
		// attempt to reclaim the pressured resource.
		reclaimed, err := nodeReclaimFunc()
		if err != nil {
			glog.Warningf("eviction manager: unexpected error when attempting to reduce %v pressure: %v", resourceToReclaim, err)
		}
		// update our local observations based on the amount reported to have been reclaimed.
		// note: this is optimistic, other things could have been still consuming the pressured resource in the interim.
		signal := resourceToSignal[resourceToReclaim]
		value, ok := observations[signal]
		if !ok {
			glog.Errorf("eviction manager: unable to find value associated with signal %v", signal)
			continue
		}
		value.available.Add(*reclaimed)

		// evaluate all current thresholds to see if with adjusted observations, we think we have met min reclaim goals
		if len(thresholdsMet(m.thresholdsMet, observations, true)) == 0 {
			return true
		}
	}
	return false
}
