package scheduler

import (
	"fmt"

	"code.google.com/p/go-uuid/uuid"
	"code.google.com/p/goprotobuf/proto"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	log "github.com/golang/glog"
	"github.com/mesos/mesos-go/mesos"
)

const (
	containerCpus = 0.25 // initial CPU allocated for executor
	containerMem  = 64   // initial MB of memory allocated for executor
)

// A struct that describes a pod task.
type PodTask struct {
	ID       string
	Pod      *api.Pod
	TaskInfo *mesos.TaskInfo
	Offer    PerishableOffer
	launched bool
	deleted  bool
	podKey   string
}

func rangeResource(name string, ports []uint64) *mesos.Resource {
	if len(ports) == 0 {
		// pod may consist of a container that doesn't expose any ports on the host
		return nil
	}
	return &mesos.Resource{
		Name:   proto.String(name),
		Type:   mesos.Value_RANGES.Enum(),
		Ranges: NewRanges(ports),
	}
}

// func NewRange(begin uint64, end uint64) *mesos.Value_Ranges {
func NewRanges(ports []uint64) *mesos.Value_Ranges {
	r := make([]*mesos.Value_Range, 0)
	for _, port := range ports {
		x := proto.Uint64(port)
		r = append(r, &mesos.Value_Range{Begin: x, End: x})
	}
	return &mesos.Value_Ranges{Range: r}
}

func (t *PodTask) hasAcceptedOffer() bool {
	return t.TaskInfo != nil && t.TaskInfo.TaskId != nil
}

func (t *PodTask) GetOfferId() string {
	if t.Offer == nil {
		return ""
	}
	return t.Offer.Details().Id.GetValue()
}

// Fill the TaskInfo in the PodTask, should be called during k8s scheduling,
// before binding.
func (t *PodTask) FillTaskInfo(offer PerishableOffer) error {
	if offer == nil || offer.Details() == nil {
		return fmt.Errorf("Nil offer for task %v", t)
	}
	details := offer.Details()
	if details == nil {
		return fmt.Errorf("Illegal offer for task %v: %v", t, offer)
	}
	if t.Offer != nil && t.Offer != offer {
		return fmt.Errorf("Offer assignment must be idempotent with task %v: %v", t, offer)
	}
	t.Offer = offer
	log.V(3).Infof("Recording offer(s) %v against pod %v", details.Id, t.Pod.Name)

	t.TaskInfo.TaskId = &mesos.TaskID{Value: proto.String(t.ID)}
	t.TaskInfo.SlaveId = details.GetSlaveId()
	t.TaskInfo.Resources = []*mesos.Resource{
		mesos.ScalarResource("cpus", containerCpus),
		mesos.ScalarResource("mem", containerMem),
	}
	if ports := rangeResource("ports", t.Ports()); ports != nil {
		t.TaskInfo.Resources = append(t.TaskInfo.Resources, ports)
	}
	return nil
}

// Clear offer-related details from the task, should be called if/when an offer
// has already been assigned to a task but for some reason is no longer valid.
func (t *PodTask) ClearTaskInfo() {
	log.V(3).Infof("Clearing offer(s) from pod %v", t.Pod.Name)
	t.Offer = nil
	t.TaskInfo.TaskId = nil
	t.TaskInfo.SlaveId = nil
	t.TaskInfo.Resources = nil
	t.TaskInfo.Data = nil
}

func (t *PodTask) Ports() []uint64 {
	ports := make([]uint64, 0)
	for _, container := range t.Pod.Spec.Containers {
		// strip all port==0 from this array; k8s already knows what to do with zero-
		// ports (it does not create 'port bindings' on the minion-host); we need to
		// remove the wildcards from this array since they don't consume host resources
		for _, port := range container.Ports {
			// HostPort is int, not uint64.
			if port.HostPort != 0 {
				ports = append(ports, uint64(port.HostPort))
			}
		}
	}

	return ports
}

func (t *PodTask) AcceptOffer(offer *mesos.Offer) bool {
	var cpus float64 = 0
	var mem float64 = 0

	// Mimic set type
	requiredPorts := make(map[uint64]struct{})
	for _, port := range t.Ports() {
		requiredPorts[port] = struct{}{}
	}

	for _, resource := range offer.Resources {
		if resource.GetName() == "cpus" {
			cpus = *resource.GetScalar().Value
		}

		if resource.GetName() == "mem" {
			mem = *resource.GetScalar().Value
		}

		if resource.GetName() == "ports" {
			for _, r := range (*resource).GetRanges().Range {
				bp := r.GetBegin()
				ep := r.GetEnd()

				for port, _ := range requiredPorts {
					log.V(2).Infof("Evaluating port range {%d:%d} %d", bp, ep, port)

					if (bp <= port) && (port <= ep) {
						delete(requiredPorts, port)
					}
				}
			}
		}
	}

	unsatisfiedPorts := len(requiredPorts)
	if unsatisfiedPorts > 0 {
		log.V(2).Infof("Could not schedule pod %s: %d ports could not be allocated", t.Pod.Name, unsatisfiedPorts)
		return false
	}

	if (cpus < containerCpus) || (mem < containerMem) {
		log.V(2).Infof("Not enough resources: cpus: %f mem: %f", cpus, mem)
		return false
	}

	return true
}

func newPodTask(ctx api.Context, pod *api.Pod, executor *mesos.ExecutorInfo) (*PodTask, error) {
	key, err := makePodKey(ctx, pod.Name)
	if err != nil {
		return nil, err
	}
	taskId := uuid.NewUUID().String()
	task := &PodTask{
		ID:       taskId,
		Pod:      pod,
		TaskInfo: new(mesos.TaskInfo),
		podKey:   key,
	}
	task.TaskInfo.Name = proto.String("PodTask")
	task.TaskInfo.Executor = executor
	return task, nil
}

/**
HACK(jdef): we're not using etcd but k8s has implemented namespace support and
we're going to try to honor that by namespacing pod keys. Hence, the following
funcs that were stolen from:
    https://github.com/GoogleCloudPlatform/kubernetes/blob/release-0.5/pkg/registry/etcd/etcd.go
**/

const PodPath = "/pods"

// makeListKey constructs etcd paths to resource directories enforcing namespace rules
func makeListKey(ctx api.Context, prefix string) string {
	key := prefix
	ns, ok := api.NamespaceFrom(ctx)
	if ok && len(ns) > 0 {
		key = key + "/" + ns
	}
	return key
}

// makeItemKey constructs etcd paths to a resource relative to prefix enforcing namespace rules.  If no namespace is on context, it errors.
func makeItemKey(ctx api.Context, prefix string, id string) (string, error) {
	key := makeListKey(ctx, prefix)
	ns, ok := api.NamespaceFrom(ctx)
	if !ok || len(ns) == 0 {
		return "", fmt.Errorf("Invalid request.  Namespace parameter required.")
	}
	if len(id) == 0 {
		return "", fmt.Errorf("Invalid request.  Id parameter required.")
	}
	key = key + "/" + id
	return key, nil
}

// makePodListKey constructs etcd paths to pod directories enforcing namespace rules.
func makePodListKey(ctx api.Context) string {
	return makeListKey(ctx, PodPath)
}

// makePodKey constructs etcd paths to pod items enforcing namespace rules.
func makePodKey(ctx api.Context, id string) (string, error) {
	return makeItemKey(ctx, PodPath, id)
}
