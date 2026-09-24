// Package podresolution resolves pod membership at admission (ADR 0034).
//
// Membership policies select pods by the `kube-vnet.system/*` stamps. When
// only the reconciler writes them, a new pod has no stamps until the
// reconciler catches up. Resolving in the apiserver's write path means a pod
// carries its stamps from the moment it exists.
//
// Nothing here resolves or writes stamps on its own: the handlers call
// controller.Resolver and the controller's stamp helpers, the same code the
// reconciler runs, so the two modes cannot disagree.
package podresolution

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/lhns/kube-vnet/internal/controller"
)

// Paths the chart's webhook configurations point at.
const (
	MutatePath   = "/mutate-v1-pod"
	ValidatePath = "/validate-v1-pod"
)

// Deps is what both handlers need.
type Deps struct {
	Resolver *controller.Resolver
	Reader   client.Reader
	NSFilter *controller.NamespaceFilter
	Decoder  admission.Decoder
	// OperatorUsername is the operator ServiceAccount's username. Its own
	// writes are neither mutated (they already carry the resolved stamps, and
	// rewriting resolved-by would hide that the reconciler stamped the pod)
	// nor validated.
	OperatorUsername string
	// NetworkWait enables the network wait for opted-in pods (ADR 0045);
	// nil means the feature is off.
	NetworkWait *NetworkWaitConfig
}

// Register serves both handlers on srv.
func Register(srv webhook.Server, d Deps) {
	srv.Register(MutatePath, &admission.Webhook{Handler: &Mutator{d}})
	srv.Register(ValidatePath, &admission.Webhook{Handler: &Validator{d}})
}

func (d *Deps) isOperator(req admission.Request) bool {
	return d.OperatorUsername != "" && req.UserInfo.Username == d.OperatorUsername
}

// podRequest is a decoded admission request: the pod as submitted, with the
// stamps it carries and those the old object carried (empty on CREATE).
type podRequest struct {
	pod       *corev1.Pod
	stamps    map[string]string
	oldStamps map[string]string
}

func (d *Deps) decode(req admission.Request) (*podRequest, error) {
	pod := &corev1.Pod{}
	if err := d.Decoder.Decode(req, pod); err != nil {
		return nil, err
	}
	r := &podRequest{pod: pod, stamps: controller.ResolutionStamps(pod), oldStamps: map[string]string{}}
	if len(req.OldObject.Raw) > 0 {
		old := &corev1.Pod{}
		if err := d.Decoder.DecodeRaw(req.OldObject, old); err != nil {
			return nil, err
		}
		r.oldStamps = controller.ResolutionStamps(old)
	}
	return r, nil
}

// untouched reports whether the request left stamp k exactly as the old
// object had it (both absent counts). The mutator owns only untouched stamps;
// the validator polices only touched ones.
func (r *podRequest) untouched(k string) bool {
	v, inNew := r.stamps[k]
	ov, inOld := r.oldStamps[k]
	return inNew == inOld && v == ov
}

// namespaceManaged reads the Namespace from cache and applies the same filter
// the reconcilers use. The namespaceSelector cannot express the
// kube-vnet/disabled annotation, so this runs even for selected namespaces.
func (d *Deps) namespaceManaged(ctx context.Context, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	managed, err := d.NSFilter.Manages(ctx, d.Reader, name)
	if err != nil {
		return false, fmt.Errorf("read namespace %q: %w", name, err)
	}
	return managed, nil
}
