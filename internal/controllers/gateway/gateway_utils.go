package gateway

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	"github.com/kong/kubernetes-ingress-controller/v2/internal/annotations"
)

// -----------------------------------------------------------------------------
// Gateway Utilities
// -----------------------------------------------------------------------------

// maxConds is the maximum number of status conditions a Gateway can have at one time.
const maxConds = 8

// isGatewayScheduled returns boolean whether or not the gateway object was scheduled
// previously by the gateway controller.
func isGatewayScheduled(gateway *gatewayv1alpha2.Gateway) bool {
	for _, cond := range gateway.Status.Conditions {
		if cond.Type == string(gatewayv1alpha2.GatewayConditionScheduled) &&
			cond.Reason == string(gatewayv1alpha2.GatewayReasonScheduled) &&
			cond.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// isGatewayReady returns boolean whether the ready condition exists
// for the given gateway object if it matches the currently known generation of that object.
func isGatewayReady(gateway *gatewayv1alpha2.Gateway) bool {
	for _, cond := range gateway.Status.Conditions {
		if cond.Type == string(gatewayv1alpha2.GatewayConditionReady) && cond.Reason == string(gatewayv1alpha2.GatewayReasonReady) && cond.ObservedGeneration == gateway.Generation {
			return true
		}
	}
	return false
}

// isGatewayInClassAndUnmanaged returns boolean if the provided combination of gateway and class
// is controlled by this controller and the gateway is configured for unmanaged mode.
func isGatewayInClassAndUnmanaged(gatewayClass *gatewayv1alpha2.GatewayClass, gateway gatewayv1alpha2.Gateway) bool {
	_, ok := annotations.ExtractUnmanagedGatewayMode(gateway.Annotations)
	return ok && gatewayClass.Spec.ControllerName == ControllerName
}

// convertListenersToListenerStatuses converts all the listeners from the given gateway
// object into ListenerStatus objects.
func convertListenersToListenerStatuses(gateway *gatewayv1alpha2.Gateway) (listenerStatuses []gatewayv1alpha2.ListenerStatus) {
	existingListenerStatuses := make(map[gatewayv1alpha2.SectionName]gatewayv1alpha2.ListenerStatus, len(gateway.Status.Listeners))
	for _, listenerStatus := range gateway.Status.Listeners {
		existingListenerStatuses[listenerStatus.Name] = listenerStatus
	}

	for _, listener := range gateway.Spec.Listeners {
		var attachedRoutes int32
		var conditions = make([]metav1.Condition, 0)
		if existingListenerStatus, ok := existingListenerStatuses[listener.Name]; ok {
			attachedRoutes = existingListenerStatus.AttachedRoutes
		}

		listenerStatuses = append(listenerStatuses, gatewayv1alpha2.ListenerStatus{
			Name:           listener.Name,
			SupportedKinds: supportedRouteGroupKinds,
			AttachedRoutes: attachedRoutes,
			Conditions: append(conditions, metav1.Condition{
				Type:               string(gatewayv1alpha2.ListenerConditionReady),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: gateway.Generation,
				LastTransitionTime: metav1.Now(),
				Reason:             string(gatewayv1alpha2.ListenerReasonReady),
				Message:            "the listener is ready and available for routing",
			}),
		})
	}

	return
}

// getRefFromPublishService splits a publish service string in the format namespace/name into a types.NamespacedName
// and verifies the contents producing an error if they don't match namespace/name format.
func getRefFromPublishService(publishService string) (types.NamespacedName, error) {
	publishServiceSplit := strings.SplitN(publishService, "/", 3)
	if len(publishServiceSplit) != 2 {
		return types.NamespacedName{}, fmt.Errorf("--publish-service expected in format 'namespace/name' but got %s", publishService)
	}
	return types.NamespacedName{
		Namespace: publishServiceSplit[0],
		Name:      publishServiceSplit[1],
	}, nil
}

// pruneGatewayStatusConds cleans out old status conditions if the Gateway currently has more
// status conditions set than the 8 maximum allowed by the Kubernetes API.
func pruneGatewayStatusConds(gateway *gatewayv1alpha2.Gateway) *gatewayv1alpha2.Gateway {
	if len(gateway.Status.Conditions) > maxConds {
		gateway.Status.Conditions = gateway.Status.Conditions[len(gateway.Status.Conditions)-maxConds:]
	}
	return gateway
}

// reconcileGatewaysIfClassMatches is a filter function to convert a list of gateways into a list
// of reconciliation requests for those gateways based on which match the given class.
func reconcileGatewaysIfClassMatches(gatewayClass client.Object, gateways []gatewayv1alpha2.Gateway) (recs []reconcile.Request) {
	for _, gateway := range gateways {
		if string(gateway.Spec.GatewayClassName) == gatewayClass.GetName() {
			recs = append(recs, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: gateway.Namespace,
					Name:      gateway.Name,
				},
			})
		}
	}
	return
}

// areAddressesEqual determines if two lists of gateway addresses have the same contents.
func areAddressesEqual(l1 []gatewayv1alpha2.GatewayAddress, l2 []gatewayv1alpha2.GatewayAddress) bool {
	return reflect.DeepEqual(l1, l2)
}

// areListenersEqual determines if two lists of gateway listeners have the same contents.
func areListenersEqual(l1 []gatewayv1alpha2.Listener, l2 []gatewayv1alpha2.Listener) bool {
	return reflect.DeepEqual(l1, l2)
}

// -----------------------------------------------------------------------------
// Gateway Utils - Watch Predicate Helpers
// -----------------------------------------------------------------------------

// isGatewayClassEventInClass produces a boolean whether or not a given event which contains
// one or more GatewayClass objects is supported by this controller according to those
// objects ControllerName.
func isGatewayClassEventInClass(log logr.Logger, watchEvent interface{}) bool {
	objs := make([]client.Object, 0, 2)
	switch e := watchEvent.(type) {
	case event.CreateEvent:
		objs = append(objs, e.Object)
	case event.DeleteEvent:
		objs = append(objs, e.Object)
	case event.GenericEvent:
		objs = append(objs, e.Object)
	case event.UpdateEvent:
		objs = append(objs, e.ObjectOld)
		objs = append(objs, e.ObjectNew)
	default:
		log.Error(fmt.Errorf("invalid type"), "received invalid event type in event handlers", "found", reflect.TypeOf(watchEvent))
		return false
	}

	for _, obj := range objs {
		gwc, ok := obj.(*gatewayv1alpha2.GatewayClass)
		if !ok {
			log.Error(fmt.Errorf("invalid type"), "received invalid object type in event handlers", "expected", "GatewayClass", "found", reflect.TypeOf(obj))
			continue
		}
		if gwc.Spec.ControllerName == ControllerName {
			return true
		}
	}

	return false
}
