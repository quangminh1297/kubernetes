/*
Copyright 2014 The Kubernetes Authors.

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

package userspace

import (
	"context"
	"errors"
	"fmt"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	//	"k8s.io/client-go/kubernetes"

	//	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/proxy"
	"k8s.io/kubernetes/pkg/proxy/util"
	nodeutil "k8s.io/kubernetes/pkg/util/node"
	"k8s.io/kubernetes/pkg/util/slice"
	metrics "k8s.io/metrics/pkg/client/clientset/versioned"
	"net"
	"reflect"
	"sync"
	"time"
)

var (
	ErrMissingServiceEntry = errors.New("missing service entry")
	ErrMissingEndpoints    = errors.New("missing endpoints")
)
var(
	/*CPU-RAM map is HW value with key which is Pod Name */
	CPUMap = make(map[string]int64)
	RAMMap = make(map[string]int64)
	/*Return PodName by Endpoint IP key*/
	ConverEndpointToPodName = make(map[string]string)
	LatencyMap = make(map[string]float64)
	EvalutionOtherEndpoint string
	MapForOtherEndpoint []string
	MapForOtherPodName []string
	MapForOtherNodeName []string
)
type affinityState struct {
	clientIP string
	//clientProtocol  api.Protocol //not yet used
	//sessionCookie   string       //not yet used
	endpoint string
	lastUsed time.Time
}

type affinityPolicy struct {
	affinityType v1.ServiceAffinity
	affinityMap  map[string]*affinityState // map client IP -> affinity info
	ttlSeconds   int
}

// LoadBalancerRR is a round-robin load balancer.
type LoadBalancerRR struct {
	lock     sync.RWMutex
	services map[proxy.ServicePortName]*balancerState
}

// Ensure this implements LoadBalancer.
var _ LoadBalancer = &LoadBalancerRR{}

type nodeEndpoints struct {
	endpoints []string
	index int
}

type balancerState struct {
	endpoints []string // a list of "ip:port" style strings
	index     int      // current index into endpoints
	affinity  affinityPolicy
	/*My-proxy-LOCALY*/
	localendpoints []string // a list of local endpoints
	localindex     int      // current index into localendpoints
	otherEndpoints map[string]*nodeEndpoints
}


func newAffinityPolicy(affinityType v1.ServiceAffinity, ttlSeconds int) *affinityPolicy {
	return &affinityPolicy{
		affinityType: affinityType,
		affinityMap:  make(map[string]*affinityState),
		ttlSeconds:   ttlSeconds,
	}
}

// NewLoadBalancerRR returns a new LoadBalancerRR.
func NewLoadBalancerRR() *LoadBalancerRR {
	return &LoadBalancerRR{
		services: map[proxy.ServicePortName]*balancerState{},
	}
}

func (lb *LoadBalancerRR) NewService(svcPort proxy.ServicePortName, affinityType v1.ServiceAffinity, ttlSeconds int) error {
	klog.V(4).Infof("LoadBalancerRR NewService %q", svcPort)
	lb.lock.Lock()
	defer lb.lock.Unlock()
	lb.newServiceInternal(svcPort, affinityType, ttlSeconds)
	return nil
}

// This assumes that lb.lock is already held.
func (lb *LoadBalancerRR) newServiceInternal(svcPort proxy.ServicePortName, affinityType v1.ServiceAffinity, ttlSeconds int) *balancerState {
	if ttlSeconds == 0 {
		ttlSeconds = int(v1.DefaultClientIPServiceAffinitySeconds) //default to 3 hours if not specified.  Should 0 be unlimited instead????
	}

	if _, exists := lb.services[svcPort]; !exists {
		lb.services[svcPort] = &balancerState{affinity: *newAffinityPolicy(affinityType, ttlSeconds), otherEndpoints: map[string]*nodeEndpoints{}}
		klog.V(4).Infof("LoadBalancerRR service %q did not exist, created", svcPort)
	} else if affinityType != "" {
		lb.services[svcPort].affinity.affinityType = affinityType
	}
	return lb.services[svcPort]
}

func (lb *LoadBalancerRR) DeleteService(svcPort proxy.ServicePortName) {
	klog.V(4).Infof("LoadBalancerRR DeleteService %q", svcPort)
	lb.lock.Lock()
	defer lb.lock.Unlock()
	delete(lb.services, svcPort)
}

// return true if this service is using some form of session affinity.
func isSessionAffinity(affinity *affinityPolicy) bool {
	// Should never be empty string, but checking for it to be safe.
	if affinity.affinityType == "" || affinity.affinityType == v1.ServiceAffinityNone {
		return false
	}
	return true
}

// ServiceHasEndpoints checks whether a service entry has endpoints.
func (lb *LoadBalancerRR) ServiceHasEndpoints(svcPort proxy.ServicePortName) bool {
	lb.lock.RLock()
	defer lb.lock.RUnlock()
	state, exists := lb.services[svcPort]
	// TODO: while nothing ever assigns nil to the map, *some* of the code using the map
	// checks for it.  The code should all follow the same convention.
	return exists && state != nil && len(state.endpoints) > 0
}

func (lb *LoadBalancerRR) NextEndpoint_V2(svcPort proxy.ServicePortName, srcAddr net.Addr, sessionAffinityReset bool) (string, error) {
	// Coarse locking is simple.  We can get more fine-grained if/when we
	// can prove it matters.
	lb.lock.Lock()
	defer lb.lock.Unlock()
	state, exists := lb.services[svcPort]
	if !exists || state == nil {
		return "", ErrMissingServiceEntry
	}
	if len(state.endpoints) == 0 {
		return "", ErrMissingEndpoints
	}
	klog.V(2).Infof("NextEndpoint for service %q, srcAddr=%v: endpoints: %+v", svcPort, srcAddr, state.endpoints)
	sessionAffinityEnabled := isSessionAffinity(&state.affinity)
	var ipaddr string
	if sessionAffinityEnabled {
		// Caution: don't shadow ipaddr
		var err error
		ipaddr, _, err = net.SplitHostPort(srcAddr.String())
		if err != nil {
			return "", fmt.Errorf("malformed source address %q: %v", srcAddr.String(), err)
		}
		if !sessionAffinityReset {
			sessionAffinity, exists := state.affinity.affinityMap[ipaddr]
			if exists && int(time.Since(sessionAffinity.lastUsed).Seconds()) < state.affinity.ttlSeconds {
				// Affinity wins.
				endpoint := sessionAffinity.endpoint
				sessionAffinity.lastUsed = time.Now()
				klog.V(0).Infof("NextEndpoint for service %q from IP %s with sessionAffinity %#v: %s", svcPort, ipaddr, sessionAffinity, endpoint)
				return endpoint, nil
			}
		}
	}
	/*CHECK ALL OUTPUT ARGUMENT*/
	EvalutionOtherEndpoint, _, _ = FindMaxvalue(CPUMap, RAMMap, LatencyMap, ConverEndpointToPodName, MapForOtherEndpoint)
	_, ScoreList, FinalScore := FindMaxvalue(CPUMap, RAMMap, LatencyMap, ConverEndpointToPodName, MapForOtherEndpoint)
	CPUEvalutionResult, RAMEvalutionResult, CPU, RAM  := LocalNodeResource(CPUMap, RAMMap, ConverEndpointToPodName, state.localendpoints)
	/**/
	var endpoint string
	tmpppp := "OFF"
	if tmpppp == "ON" {
		if len(state.localendpoints) == 0 {
			endpoint = state.endpoints[state.index]
			state.index = (state.index + 1) % len(state.endpoints)
		} else {
			/*
				state.localindex: The number of Endpoint inside map
				state.localendpoints : local Endpoint of Node which is received requested
				ConverEndpointToPodName : Map of PodName, Key is Endpoint IP <map[EndpointIP]=PodName>
				CPUMap, RAMMap: Map contain HW value(RAM/RESOURCE), Key is PodName <map[PodName]=Value(Int64)>
			*/
			endpoint = state.localendpoints[state.localindex]
			state.localindex = (state.localindex + 1) % len(state.localendpoints)
			klog.V(0).Infof("********************************************************************")
			klog.V(0).Infof("<<< state.localindex >>>", state.localindex)
			klog.V(0).Infof("<<< state.localendpoints >>>", state.localendpoints)
			klog.V(0).Infof("<<< LEN OF state.localendpoints >>>", len(state.localendpoints))
			klog.V(0).Infof("<<< LIST ConverEndpointToPodName - NextEndpoint >>>", ConverEndpointToPodName)
			klog.V(0).Infof("<<< LEN OF ConverEndpointToPodName - NextEndpoint  >>>", len(ConverEndpointToPodName))
			klog.V(0).Infof("<<< hostname ofMetricSource-CPUMap >>>", CPUMap)
			klog.V(0).Infof("<<< hostname ofMetricSource-RAMMap >>>", RAMMap)
			klog.V(0).Infof("<<< hostname ofMetricSource-ResultOfCPU: >>>", CPUEvalutionResult)
			klog.V(0).Infof("<<< hostname ofMetricSource-ResultOfRAM: >>>", RAMEvalutionResult)
			klog.V(0).Infof("<<< ********************** NEXT ENDPOINT *************************** >>>")
		}
	}
	if len(state.localendpoints) != 0 && CPUEvalutionResult == true && RAMEvalutionResult == true{
		endpoint = state.localendpoints[state.localindex]
		state.localindex = (state.localindex + 1) % len(state.localendpoints)

		ahihi_test := state.otherEndpoints["worker03"].endpoints[state.otherEndpoints["worker03"].index]
		state.otherEndpoints["worker03"].index = (state.otherEndpoints["worker03"].index + 1) % len(state.otherEndpoints["worker03"].endpoints)
		klog.V(0).Infof("Check Index of Func: ", ahihi_test)
		klog.V(0).Infof("MapForOtherEndpoint", MapForOtherEndpoint)
		/*
			state.localindex: The number of Endpoint inside map
			state.localendpoints : local Endpoint of Node which is received requested
			ConverEndpointToPodName : Map of PodName, Key is Endpoint IP <map[EndpointIP]=PodName>
			CPUMap, RAMMap: Map contain HW value(RAM/RESOURCE), Key is PodName <map[PodName]=Value(Int64)>
		*/
	}else {
		//EvalutionOtherEndpoint := FindMaxvalue(CPUMap, RAMMap, LatencyMap, ConverEndpointToPodName, MapForOtherEndpoint)
		ahihi_test := state.otherEndpoints["worker03"].endpoints[state.otherEndpoints["worker03"].index]
		state.otherEndpoints["worker03"].index = (state.otherEndpoints["worker03"].index + 1) % len(state.otherEndpoints["worker03"].endpoints)
		klog.V(0).Infof("Check Index of Func-22: ", ahihi_test)


		endpoint = state.endpoints[state.index]
		state.index = (state.index + 1) % len(state.endpoints)
	}
	/*END*/
	if sessionAffinityEnabled {
		var affinity *affinityState
		affinity = state.affinity.affinityMap[ipaddr]
		if affinity == nil {
			affinity = new(affinityState) //&affinityState{ipaddr, "TCP", "", endpoint, time.Now()}
			state.affinity.affinityMap[ipaddr] = affinity
		}
		affinity.lastUsed = time.Now()
		affinity.endpoint = endpoint
		affinity.clientIP = ipaddr
		klog.V(0).Infof("Updated affinity key %s: %#v", ipaddr, state.affinity.affinityMap[ipaddr])
	}
	//klog.V(0).Infof("<<< NEXT-ENDPOINT: %q >>>", endpoint)
	go PrintLogPerMinutes(state.endpoints, CPU, RAM, CPUMap, RAMMap, CPUEvalutionResult, RAMEvalutionResult, EvalutionOtherEndpoint, ScoreList, FinalScore, endpoint)
	return endpoint, nil
}
func PrintLogPerMinutes(RangeOfEndpoint []string, PersenOfCPU float64, PersenOfRAM float64, CPUMap map[string]int64, RAMMap map[string]int64, ResultCPU bool, ResultRAM bool, EvalutionOtherEndpoint string, ListOfScore map[string]float64, score01 float64, CurrentEndpoint string){
	for range RangeOfEndpoint {
		klog.V(0).Infof("<<< ********************** NEXT ENDPOINT - LOCALLY *************************** >>>")
		klog.V(0).Infof("*** Current Endpoint", CurrentEndpoint)
		klog.V(0).Infof("*** Percentages of CPU", PersenOfCPU)
		klog.V(0).Infof("*** Result of CPU", ResultCPU)
		klog.V(0).Infof("*** Percentages of RAM", PersenOfRAM)
		klog.V(0).Infof("*** Result of RAM", ResultRAM)
		klog.V(0).Infof("*** Map of PodName Per CPU", CPUMap)
		klog.V(0).Infof("*** Map of PodName Per RAM", RAMMap)
		klog.V(0).Infof("<<< ********************** NEXT ENDPOINT - ORTER *************************** >>>")
		klog.V(0).Infof("*** List Of Score:", ListOfScore)
		klog.V(0).Infof("*** Best Score: ", score01)
		klog.V(0).Infof("*** READY ENDPOINT FOR OVERLOAD:", EvalutionOtherEndpoint)
		klog.V(0).Infof("<<< ********************** NEXT ENDPOINT*************************** >>>")
		time.Sleep(20 * time.Second)
	}
}
func FindMaxvalue(CPUMapForMax map[string]int64, RAMMapForMax map[string]int64, LatencyMapForMin map[string]float64, ConvertIPtoPodName map[string]string, EndPontIP_map []string) (string, map[string]float64, float64) {
	//klog.V(0).Infof("<<< Check Input Of FindMaxvalue-CPUMapForMax >>>", CPUMapForMax)
	//klog.V(0).Infof("<<< Check Input Of FindMaxvalue-RAMMapForMax >>>", RAMMapForMax)
	//klog.V(0).Infof("<<< Check Input Of FindMaxvalue-LatencyMapForMin >>>", LatencyMapForMin)
	//klog.V(0).Infof("<<< Check Input Of FindMaxvalue-EndPontIP_map >>>", EndPontIP_map)
	//klog.V(0).Infof("<<< Check Input Of FindMaxvalue-ConvertIPtoPodName >>>", ConvertIPtoPodName)
	var(
		Evalution_Result_key float64
		FINAL_RESULT string
	)
	DevicedValue_CPU := make(map[string]float64)
	DevicedValue_RAM := make(map[string]float64)
	DevicedValue_Latency := make(map[string]float64)
	pod_Choose := make(map[float64]string)
	ListTotalEvalution := make(map[string]float64)

	maxvalue_CPU := CPUMapForMax[ConvertIPtoPodName[EndPontIP_map[0]]]
	maxvalue_RAM := RAMMapForMax[ConvertIPtoPodName[EndPontIP_map[0]]]
	MinValue_Latency := LatencyMapForMin[ConvertIPtoPodName[EndPontIP_map[0]]]

	for tmpCPU := range EndPontIP_map{
		if CPUMapForMax[ConvertIPtoPodName[EndPontIP_map[tmpCPU]]] > maxvalue_CPU{
			maxvalue_CPU = CPUMapForMax[ConvertIPtoPodName[EndPontIP_map[tmpCPU]]]
		}
	}

	for tmpRAM := range EndPontIP_map{
		if RAMMapForMax[ConvertIPtoPodName[EndPontIP_map[tmpRAM]]] > maxvalue_RAM{
			maxvalue_RAM = RAMMapForMax[ConvertIPtoPodName[EndPontIP_map[tmpRAM]]]
		}
	}

	for tmpLatency := range EndPontIP_map{
		if LatencyMapForMin[ConvertIPtoPodName[EndPontIP_map[tmpLatency]]] < MinValue_Latency{
			MinValue_Latency = LatencyMapForMin[ConvertIPtoPodName[EndPontIP_map[tmpLatency]]]
		}
	}

	for tmp02 := range EndPontIP_map{
		DevicedValue_Latency[ConvertIPtoPodName[EndPontIP_map[tmp02]]] = float64(MinValue_Latency)/float64(LatencyMapForMin[ConvertIPtoPodName[EndPontIP_map[tmp02]]])
		DevicedValue_RAM[ConvertIPtoPodName[EndPontIP_map[tmp02]]] = float64(RAMMapForMax[ConvertIPtoPodName[EndPontIP_map[tmp02]]])/float64(maxvalue_RAM)
		DevicedValue_CPU[ConvertIPtoPodName[EndPontIP_map[tmp02]]] = float64(CPUMapForMax[ConvertIPtoPodName[EndPontIP_map[tmp02]]])/float64(maxvalue_CPU)
		ListTotalEvalution[ConvertIPtoPodName[EndPontIP_map[tmp02]]] = DevicedValue_Latency[ConvertIPtoPodName[EndPontIP_map[tmp02]]] + DevicedValue_RAM[ConvertIPtoPodName[EndPontIP_map[tmp02]]] + DevicedValue_CPU[ConvertIPtoPodName[EndPontIP_map[tmp02]]]
		pod_Choose[ListTotalEvalution[ConvertIPtoPodName[EndPontIP_map[tmp02]]]] = ConvertIPtoPodName[EndPontIP_map[tmp02]]
	}

	tmp_Total_Evalution_key := ListTotalEvalution[ConvertIPtoPodName[EndPontIP_map[0]]]
	for tmp := range EndPontIP_map {
		if ListTotalEvalution[ConvertIPtoPodName[EndPontIP_map[tmp]]] >= tmp_Total_Evalution_key {
			tmp_Total_Evalution_key = ListTotalEvalution[ConvertIPtoPodName[EndPontIP_map[tmp]]]
			Evalution_Result_key = tmp_Total_Evalution_key
		}
	}
	FINAL_RESULT = pod_Choose[Evalution_Result_key]
	klog.V(0).Infof("ListTotalEvalution: ", ListTotalEvalution)
	klog.V(0).Infof("Pod choose for NEXT ENDPOINT: ", FINAL_RESULT)
	klog.V(0).Infof("Pod's Evalution Point", Evalution_Result_key)
	/* ADD tmp RETURN: ListTotalEvalution-Evalution_Result_key*/
	return FINAL_RESULT, ListTotalEvalution, Evalution_Result_key
}
/* This function is created by NGUYEN QUANG MINH - Support calculating Finnal result by CPU/RAM resource of Local Worker-Node*/
/*Input xxx*/
func LocalNodeResource(MapOfCPU map[string]int64, MapOfRAM map[string]int64, ConverEToNN map[string]string, EndpointMap []string) (bool,bool, float64, float64){
	//klog.V(0).Infof("<<< Check Input Of MapOfCPU - LocalNodeResource>>>", MapOfCPU)
	//klog.V(0).Infof("<<< Check Input Of MapOfRAM - LocalNodeResource>>>", MapOfRAM)
	//klog.V(0).Infof("<<< Check Input Of ConverEToNN - LocalNodeResource>>>", ConverEToNN)
	//klog.V(0).Infof("<<< Check Input Of EndpointMap - LocalNodeResource>>>", EndpointMap)
	var (
	Total_Custom_CPU int64
	Total_Custom_RAM int64
	Average_CPU float64
	Average_RAM float64
	Usage_Perventage_CPU float64
	Usage_Perventage_RAM float64
	ResultOfCPU bool
	ResultOfRAM bool
	)

	NumberOfObject := len(EndpointMap)
	Limitation_Value := 500
	/*Process for Average*/
	for tmp1 := range (EndpointMap){
		Total_Custom_CPU += MapOfCPU[ConverEToNN[EndpointMap[tmp1]]]
		Total_Custom_RAM += MapOfRAM[ConverEToNN[EndpointMap[tmp1]]]
	}
	//klog.V(0).Infof("############################################################")
	Average_CPU = float64(Total_Custom_CPU)/float64(NumberOfObject)
	Usage_Perventage_CPU =(Average_CPU*100)/float64(Limitation_Value)
	Average_RAM = float64(Total_Custom_RAM)/float64(NumberOfObject)
	Usage_Perventage_RAM = (Average_RAM*100)/float64(Limitation_Value)

	/*
	HW Resource must more than 20%
	if Usage_Perventage_CPU > 20 fixed
	*/
	if Usage_Perventage_CPU < 20{
		//klog.V(0).Infof("CPU of Local Worker is *OK* - Usage %v", Usage_Perventage_CPU)
		ResultOfCPU = true
	}else {
		//klog.V(0).Infof("CPU of Local Worker is *NOT_OK* - Usage %v", Usage_Perventage_CPU)
		ResultOfCPU = false
	}
	if Usage_Perventage_RAM < 20{
		//klog.V(0).Infof("RAM of Local Worker is *OK* - Usage %v", Usage_Perventage_RAM)
		ResultOfRAM = true
	}else {
		//klog.V(0).Infof("RAM of Local Worker is *NOT_OK* - Usage %v", Usage_Perventage_RAM)
		ResultOfRAM = false
	}
	klog.V(0).Infof("CALCULATION HW: Total__CPU: %v - Total__RAM: %v", Total_Custom_CPU, Total_Custom_RAM)
	//klog.V(0).Infof("############################################################")
	/* ADD tmp RETURN: Usage_Perventage_CPU-Usage_Perventage_RAM*/
	return ResultOfCPU, ResultOfRAM, Usage_Perventage_CPU, Usage_Perventage_RAM
}

// Remove any session affinity records associated to a particular endpoint (for example when a pod goes down).
func removeSessionAffinityByEndpoint(state *balancerState, svcPort proxy.ServicePortName, endpoint string) {
	for _, affinity := range state.affinity.affinityMap {
		if affinity.endpoint == endpoint {
			klog.V(4).Infof("Removing client: %s from affinityMap for service %q", affinity.endpoint, svcPort)
			delete(state.affinity.affinityMap, affinity.clientIP)
		}
	}
}

// Loop through the valid endpoints and then the endpoints associated with the Load Balancer.
// Then remove any session affinity records that are not in both lists.
// This assumes the lb.lock is held.
func (lb *LoadBalancerRR) removeStaleAffinity(svcPort proxy.ServicePortName, newEndpoints []string) {
	newEndpointsSet := sets.NewString()
	for _, newEndpoint := range newEndpoints {
		newEndpointsSet.Insert(newEndpoint)
	}

	state, exists := lb.services[svcPort]
	if !exists {
		return
	}
	for _, existingEndpoint := range state.endpoints {
		if !newEndpointsSet.Has(existingEndpoint) {
			klog.V(2).Infof("Delete endpoint %s for service %q", existingEndpoint, svcPort)
			removeSessionAffinityByEndpoint(state, svcPort, existingEndpoint)
		}
	}
}

func (lb *LoadBalancerRR) OnEndpointsAdd(endpoints *v1.Endpoints) {
	portsToEndpoints := util.BuildPortsToEndpointsMap(endpoints)
	portsToNodeNames, portsToPodNames := util.BuildPortsToNodeNamesMap(endpoints)
	hostname, err := nodeutil.GetHostname("")
	if err != nil {
		klog.V(1).Infof("Roudrobin: Couldn't determine hostname")
	}
	lb.lock.Lock()
	defer lb.lock.Unlock()
	for portname := range portsToEndpoints {
		svcPort := proxy.ServicePortName{NamespacedName: types.NamespacedName{Namespace: endpoints.Namespace, Name: endpoints.Name}, Port: portname}
		newEndpoints := portsToEndpoints[portname]
		nodenames := portsToNodeNames[portname] /*LOCALT*/
		PodNames := portsToPodNames[portname]
		state, exists := lb.services[svcPort]
		if !exists || state == nil || len(newEndpoints) > 0 {
			klog.V(0).Infof("Roudrobin: Setting endpoints for %s to %+v", svcPort, newEndpoints)
			// OnEndpointsAdd can be called without NewService being called externally.
			// To be safe we will call it here.  A new service will only be created
			// if one does not already exist.
			state = lb.newServiceInternal(svcPort, v1.ServiceAffinity(""), 0)
			state.localendpoints = nil /*LOCALT*/
			for j := range newEndpoints {
				ep, ok := state.otherEndpoints[nodenames[j]]
				klog.V(0).Infof("<<< ep-check: >>> -->", ep)
				if ok {
					state.otherEndpoints[nodenames[j]].endpoints = nil
				}
			}
			for i := range newEndpoints {
				if len(PodNames) != 0 {
					ConverEndpointToPodName[newEndpoints[i]] = PodNames[i]
				}
				if nodenames[i] == hostname {
					state.localendpoints = append(state.localendpoints, newEndpoints[i])
				} else {
					ep, ok := state.otherEndpoints[nodenames[i]]
					klog.V(0).Infof("<<< State of Endpoint: >>>", ep)
					if ok {
						state.otherEndpoints[nodenames[i]].endpoints = append(state.otherEndpoints[nodenames[i]].endpoints, newEndpoints[i])
					} else {
						state.otherEndpoints[nodenames[i]] = &nodeEndpoints{endpoints: []string{newEndpoints[i]}, index: 0}
					}
					klog.V(0).Infof(" <<< OnEndpointsAdd - state.otherEndpoints[nodenames[i]] %+v --- %+v  >>> ", state.otherEndpoints[nodenames[i]], nodenames[i])
					klog.V(0).Infof(" <<< OnEndpointsAdd - state.otherEndpoints[nodenames[i]].endpoint %+v --- %+v  >>> ", state.otherEndpoints[nodenames[i]].endpoints, nodenames[i])
				}
			}
			klog.V(0).Infof("OnEndpointsAdd: service %s --- %+v --- %+v", portname, state.localendpoints, state.otherEndpoints)
			state.index = 0
			state.localindex = 0 /*LOCALT*/
		}
	}
}
func (lb *LoadBalancerRR) OnEndpointsUpdate(oldEndpoints, endpoints *v1.Endpoints) {
	portsToEndpoints := util.BuildPortsToEndpointsMap(endpoints)
	oldPortsToEndpoints := util.BuildPortsToEndpointsMap(oldEndpoints)
	registeredEndpoints := make(map[proxy.ServicePortName]bool)
	/*LOCALT*/
	portsToNodeNames, portsToPodNames := util.BuildPortsToNodeNamesMap(endpoints)
	hostname, err := nodeutil.GetHostname("")
	if err != nil {
		klog.V(1).Infof("LoadBalancerRR: Couldn't determine hostname")
	}
	/*END*/
	lb.lock.Lock()
	defer lb.lock.Unlock()
	for portname := range portsToEndpoints {
		svcPort := proxy.ServicePortName{NamespacedName: types.NamespacedName{Namespace: endpoints.Namespace, Name: endpoints.Name}, Port: portname}
		newEndpoints := portsToEndpoints[portname]
		nodenames := portsToNodeNames[portname] //local
		PodNames := portsToPodNames[portname]
		state, exists := lb.services[svcPort]
		curEndpoints := []string{}
		//go MonitorMetricCustom(newEndpoints)
		go MonitorMetricCustom(newEndpoints)
		if state != nil {
			curEndpoints = state.endpoints
		}
		if !exists || state == nil || len(curEndpoints) != len(newEndpoints) || !slicesEquiv(slice.CopyStrings(curEndpoints), newEndpoints) {
			klog.V(0).Infof(" Setting endpoints for %s to %+v", svcPort, newEndpoints)
			lb.removeStaleAffinity(svcPort, newEndpoints)
			// OnEndpointsUpdate can be called without NewService being called externally.
			// To be safe we will call it here.  A new service will only be created
			// if one does not already exist.  The affinity will be updated
			// later, once NewService is called.
			state = lb.newServiceInternal(svcPort, v1.ServiceAffinity(""), 0)
			state.endpoints = util.ShuffleStrings(newEndpoints) // original

			/**LOCAL**/
			state.localendpoints = nil
			MapForOtherEndpoint = nil
			MapForOtherNodeName = nil
			MapForOtherPodName = nil
			for j := range newEndpoints {
				ep, ok := state.otherEndpoints[nodenames[j]]
				klog.V(0).Infof("<<< ep-check: >>> -->", ep)
				if ok {
					state.otherEndpoints[nodenames[j]].endpoints = nil
				}
			}
			for i := range newEndpoints {
				if len(PodNames) != 0 {
					ConverEndpointToPodName[newEndpoints[i]] = PodNames[i]
				}
				if nodenames[i] == hostname {
					state.localendpoints = append(state.localendpoints, newEndpoints[i])
				} else {
					ep, ok := state.otherEndpoints[nodenames[i]]
					klog.V(0).Infof("<<< ep-check >>>", ep)
					if ok {
						state.otherEndpoints[nodenames[i]].endpoints = append(state.otherEndpoints[nodenames[i]].endpoints, newEndpoints[i])
					} else {
						state.otherEndpoints[nodenames[i]] = &nodeEndpoints{endpoints: []string{newEndpoints[i]}, index: 0}
					}
					hellu := state.endpoints
					klog.V(0).Infof("OnEndpointsUpdate: Get All OtherEndpoint %s", hellu)
					klog.V(0).Infof("OnEndpointsUpdate: hostname %s", hostname)
					MapForOtherEndpoint = append(MapForOtherEndpoint, newEndpoints[i])
					MapForOtherNodeName = append(MapForOtherNodeName, nodenames[i])
					klog.V(0).Infof("OnEndpointsUpdate: Get All MapForOtherEndpoint %s", MapForOtherEndpoint)
					klog.V(0).Infof("OnEndpointsUpdate: Get All MapForOtherNodeName %s", MapForOtherNodeName)
					if len(PodNames) != 0 {
						MapForOtherPodName = append(MapForOtherPodName, PodNames[i])
						klog.V(0).Infof("OnEndpointsUpdate: Get All MapForOtherPodName %s", MapForOtherPodName)
						abc := LatencyListWithPodName(hostname, nodenames[i], PodNames[i])
						klog.V(0).Infof("OnEndpointsUpdate: Get LatencyListWithPodName ", abc)
					}
					klog.V(0).Infof(" <<< OnEndpointsUpdate - state.otherEndpoints[nodenames[i]] %+v OF NODENAMES %+v  >>> ", state.otherEndpoints[nodenames[i]], nodenames[i])
					klog.V(0).Infof(" <<< OnEndpointsUpdate - state.otherEndpoints[nodenames[i]].endpoint %+v OF NODENAMES %+v  >>> ", state.otherEndpoints[nodenames[i]].endpoints, nodenames[i])
				}
				/* Don't print state.localendpoints or state.localendpoint.nodename in here ==> Missing Endpoint Error*/
			}
			klog.V(0).Infof("OnEndpointsUpdate: service %s --- %+v --- %+v", portname, state.localendpoints, state.otherEndpoints)
			/*END*/
			state.index = 0
			state.localindex = 0
		}
		registeredEndpoints[svcPort] = true
	}
	// Now remove all endpoints missing from the update.
	for portname := range oldPortsToEndpoints {
		svcPort := proxy.ServicePortName{NamespacedName: types.NamespacedName{Namespace: oldEndpoints.Namespace, Name: oldEndpoints.Name}, Port: portname}
		if _, exists := registeredEndpoints[svcPort]; !exists {
			lb.resetService(svcPort)
		}
	}
}

func LatencyListWithPodName(LocalHostName string, map_NodeName string, map_PodName string) map[string]float64{
	klog.V(0).Infof("<<< Check Input Of LatencyListWithPodName-LocalHostName >>>", LocalHostName)
	klog.V(0).Infof("<<< Check Input Of LatencyListWithPodName-map_NodeName >>>", map_NodeName)
	klog.V(0).Infof("<<< Check Input Of LatencyListWithPodName-map_PodName >>>", map_PodName)
	MatrixLatency := make(map[string]map[string]float64)
	MatrixLatency["worker01"] = map[string]float64{
		"worker02": 4,
		"worker03": 8,
	}
	MatrixLatency["worker02"] = map[string]float64{
		"worker01": 4,
		"worker03": 3,
	}
	MatrixLatency["worker03"] = map[string]float64{
		"worker01": 8,
		"worker02": 3,
	}
	LatencyMap[map_PodName] = MatrixLatency[LocalHostName][map_NodeName]
	klog.V(0).Infof("MapOfLatencyPerPOD:", LatencyMap)
	return LatencyMap
}

func (lb *LoadBalancerRR) resetService(svcPort proxy.ServicePortName) {
	// If the service is still around, reset but don't delete.
	if state, ok := lb.services[svcPort]; ok {
		if len(state.endpoints) > 0 {
			klog.V(2).Infof("LoadBalancerRR: Removing endpoints for %s", svcPort)
			state.endpoints = []string{}
		}
		state.index = 0
		state.affinity.affinityMap = map[string]*affinityState{}
	}
}

func (lb *LoadBalancerRR) OnEndpointsDelete(endpoints *v1.Endpoints) {
	portsToEndpoints := util.BuildPortsToEndpointsMap(endpoints)

	lb.lock.Lock()
	defer lb.lock.Unlock()

	for portname := range portsToEndpoints {
		svcPort := proxy.ServicePortName{NamespacedName: types.NamespacedName{Namespace: endpoints.Namespace, Name: endpoints.Name}, Port: portname}
		lb.resetService(svcPort)
	}
}

func (lb *LoadBalancerRR) OnEndpointsSynced() {
}

// Tests whether two slices are equivalent.  This sorts both slices in-place.
func slicesEquiv(lhs, rhs []string) bool {
	if len(lhs) != len(rhs) {
		return false
	}
	if reflect.DeepEqual(slice.SortStrings(lhs), slice.SortStrings(rhs)) {
		return true
	}
	return false
}

func (lb *LoadBalancerRR) CleanupStaleStickySessions(svcPort proxy.ServicePortName) {
	lb.lock.Lock()
	defer lb.lock.Unlock()

	state, exists := lb.services[svcPort]
	if !exists {
		return
	}
	for ip, affinity := range state.affinity.affinityMap {
		if int(time.Since(affinity.lastUsed).Seconds()) >= state.affinity.ttlSeconds {
			klog.V(4).Infof("Removing client %s from affinityMap for service %q", affinity.clientIP, svcPort)
			delete(state.affinity.affinityMap, ip)
		}
	}
}


func MonitorMetricCustom(LimitLoop []string){
	for range LimitLoop{
		config, err0 := clientcmd.BuildConfigFromFlags("", "/home/config")
		if err0 != nil {
			panic(err0)
			klog.V(0).Infof("<<< err0 : %s >>>", err0)
		}
		mc, err1 := metrics.NewForConfig(config)
		if err1 != nil {
			panic(err1)
			klog.V(0).Infof("<<< err1 : %s >>>", err1)
		}
		podMetrics, _ := mc.MetricsV1beta1().PodMetricses(metav1.NamespaceDefault).List(context.TODO(), metav1.ListOptions{})
		for _, podMetric := range podMetrics.Items {
			containerMetrics := podMetric.Containers
			klog.V(0).Infof("<<< containerMetrics-LEN >>>", len(containerMetrics))
			MetricSource := podMetric.ObjectMeta
			for _, containerMetric := range containerMetrics {
				//containerCPUUsage := containerMetric.Usage.Cpu().String()
				//containerRAMUsage := containerMetric.Usage.Memory().String()
				//	containerName := containerMetric.Name
				containerCPUUsage_conveter := containerMetric.Usage.Cpu().MilliValue()
				containerRAMUsage_conveter := (containerMetric.Usage.Memory().Value() / 1024 / 1024)
				stupid_key := MetricSource.Name
				CPUMap[stupid_key] = (500 - containerCPUUsage_conveter)
				RAMMap[stupid_key] = (500 - containerRAMUsage_conveter)
			}
		}
		klog.V(0).Infof("<<< +++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++ >>>")
		klog.V(0).Infof("<<< Outside ofMetricSource-CPUMap >>>", CPUMap)
		klog.V(0).Infof("<<< Outside ofMetricSource-RAMMap >>>", RAMMap)
		klog.V(0).Infof("<<< +++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++ >>>")
		time.Sleep(60 * time.Second)
	}
}

